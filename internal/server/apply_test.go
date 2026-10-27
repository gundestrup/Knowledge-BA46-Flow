package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/review"
)

func TestApplyAPIExposesPersistedFailureAndBatch(t *testing.T) {
	_, db, srv := setup(t)
	defer srv.Close()
	docs, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), docs[0], "fake", "", "", nil, []database.Suggestion{{
		Action: "expand", ProposedContent: "# Wrong replacement",
	}}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.PendingSuggestions(t.Context())
	// Seed a previously accepted invalid row, as in the reported incident.
	if err := db.Decide(t.Context(), rows[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(srv.URL+"/api/apply", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var results []review.ApplyResult
	if err := json.NewDecoder(response.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(results) != 1 || results[0].Status != "failed" {
		t.Fatalf("response=%d results=%+v", response.StatusCode, results)
	}
	var detail database.Suggestion
	getJSON(t, srv.URL+"/api/suggestions/"+strconv.FormatInt(rows[0].ID, 10), &detail)
	if detail.Error == "" || detail.ApplyBatchID == "" || detail.ApplyAttemptedAt == "" ||
		detail.ApplyBatchID != results[0].BatchID || !strings.Contains(detail.Error, "body text") {
		t.Fatalf("missing diagnostic evidence: %+v", detail)
	}
	var failed []database.Suggestion
	getJSON(t, srv.URL+"/api/suggestions?status=failed", &failed)
	if len(failed) != 1 || failed[0].Error != detail.Error {
		t.Fatalf("failure reason missing from list: %+v", failed)
	}
}
