package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/review"
)

func TestQueueReconciliationRequiresCurrentExplicitConfirmation(t *testing.T) {
	c, db, previous := setup(t)
	previous.Close()
	c.WriteScope = ""
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()
	docs, _ := db.Documents(t.Context())
	if err := db.SaveLinkFix(t.Context(), docs[0], []database.Suggestion{{
		Action: "expand", Origin: "deterministic", ProposedContent: "# Wrong replacement\n",
	}}); err != nil {
		t.Fatal(err)
	}
	var plan review.Reconciliation
	getJSON(t, srv.URL+"/api/reconcile", &plan)
	if plan.RetireCount != 1 || plan.Counts["blocked"] != 1 {
		t.Fatalf("plan=%+v", plan)
	}
	var listed []database.Suggestion
	getJSON(t, srv.URL+"/api/suggestions", &listed)
	if len(listed) != 2 || listed[0].ReviewState != "blocked" || len(listed[0].ChangeTypes) == 0 || listed[0].ProposedContent != "" {
		t.Fatalf("missing review facets/preflight or leaked large payload: %+v", listed)
	}
	post := func(body string, want int) {
		t.Helper()
		response, err := http.Post(srv.URL+"/api/reconcile", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			var detail map[string]any
			_ = json.NewDecoder(response.Body).Decode(&detail)
			t.Fatalf("body=%s status=%d want=%d response=%v", body, response.StatusCode, want, detail)
		}
	}
	post(`{}`, http.StatusBadRequest)
	post(`{"confirm":false,"token":"`+plan.Token+`"}`, http.StatusBadRequest)
	post(`{"confirm":true,"token":"outdated"}`, http.StatusConflict)
	post(`{"confirm":true,"token":"`+plan.Token+`"}{}`, http.StatusBadRequest)
	post(`{"confirm":true,"token":"`+plan.Token+`"}`, http.StatusOK)
	post(`{"confirm":true,"token":"`+plan.Token+`"}`, http.StatusConflict)
	active, _ := db.ActiveSuggestions(t.Context())
	if len(active) != 1 || active[0].Action != "move" || active[0].Status != "pending" {
		t.Fatalf("policy-blocked proposal was lost: %+v", active)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM review_events WHERE actor='system' AND classification='invalid'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if err != nil || string(raw) != "# doc" {
		t.Fatalf("reconciliation changed a note: %q %v", raw, err)
	}
}

func TestReconciliationAndAcceptanceHonorRuntimeScope(t *testing.T) {
	c, db, previous := setup(t)
	previous.Close()
	c.WriteScope = ""
	srv := httptest.NewServer(NewMux(c, db, nil))
	defer srv.Close()
	response, err := http.Post(srv.URL+"/api/suggestions/1/decide", "application/json", strings.NewReader(`{"status":"accepted"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatal("HTTP acceptance ignored write scope")
	}
	// Accepted with an old binary, before the restrictive policy existed.
	if err := db.Decide(t.Context(), 1, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	response, err = http.Post(srv.URL+"/api/apply", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var results []review.ApplyResult
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(results) != 1 || results[0].Status != "blocked" || results[0].Applied {
		t.Fatalf("legacy acceptance bypassed scope: %+v", results)
	}
	response, err = http.Post(srv.URL+"/api/suggestions/1/decide", "application/json", strings.NewReader(`{"status":"pending"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	current, _ := db.Suggestion(t.Context(), 1)
	if response.StatusCode != http.StatusOK || current.Status != "pending" {
		t.Fatalf("withdrawal failed: %+v", current)
	}
}
