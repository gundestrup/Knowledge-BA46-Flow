package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"knowledge-ba46-flow/internal/review"
)

func (o *ops) previewReconcile(w http.ResponseWriter, r *http.Request) {
	plan, err := review.PreviewReconciliation(r.Context(), o.c, o.db)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (o *ops) confirmReconcile(w http.ResponseWriter, r *http.Request) {
	if !o.begin() {
		writeErr(w, http.StatusConflict, fmt.Errorf("another operation is already running"))
		return
	}
	defer o.end()
	var body struct {
		Confirm bool   `json:"confirm"`
		Token   string `json:"token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || !body.Confirm || body.Token == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("explicit confirm=true and the current preview token are required"))
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("exactly one JSON confirmation object is required"))
		return
	}
	plan, err := review.ConfirmReconciliation(r.Context(), o.c, o.db, body.Token)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}
