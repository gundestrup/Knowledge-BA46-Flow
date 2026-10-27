package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	repositoryreport "knowledge-ba46-flow/internal/report"
)

type landingResponse struct {
	Knowledge landingKnowledge `json:"knowledge"`
	Report    landingReport    `json:"report"`
}

type landingKnowledge struct {
	ChangedFiles         int    `json:"changed_files"`
	ModelDrift           int    `json:"model_drift"`
	ExternalChangedFiles int    `json:"external_changed_files"`
	ExternalModelDrift   int    `json:"external_model_drift"`
	LastCycleAt          string `json:"last_cycle_at"`
	DaysSinceLastCycle   *int   `json:"days_since_last_cycle"`
}

type landingReport struct {
	Configured        bool                        `json:"configured"`
	RemoteEnabled     bool                        `json:"remote_enabled"`
	GeneratedAt       string                      `json:"generated_at,omitempty"`
	RemoteGeneratedAt string                      `json:"remote_generated_at,omitempty"`
	Summary           repositoryreport.Summary    `json:"summary"`
	Comparison        repositoryreport.Comparison `json:"comparison"`
}

func landingData(c config.Config, db *database.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		localModel, err := c.TaskModel(config.TaskAnalyze, false, "")
		if err != nil {
			localModel = c.OllamaModel
		}
		externalModel, _ := c.TaskModel(config.TaskAnalyze, true, "")
		knowledge, err := db.KnowledgeDashboard(r.Context(), localModel, externalModel)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		result := landingResponse{
			Knowledge: landingKnowledge{
				ChangedFiles:         knowledge.ChangedFiles,
				ModelDrift:           knowledge.ModelDrift,
				ExternalChangedFiles: knowledge.ExternalChangedFiles,
				ExternalModelDrift:   knowledge.ExternalModelDrift,
				LastCycleAt:          knowledge.LastCycleAt,
			},
			Report: landingReport{
				Configured:    c.Report.RegistryPath != "",
				RemoteEnabled: c.Report.WantsRemote(),
			},
		}
		if knowledge.LastCycleAt != "" {
			if finished, err := time.Parse(time.RFC3339, knowledge.LastCycleAt); err == nil {
				days := int(time.Since(finished) / (24 * time.Hour))
				if days < 0 {
					days = 0
				}
				result.Knowledge.DaysSinceLastCycle = &days
			}
		}

		if result.Report.Configured {
			_, data, err := db.RepoReportSnapshot(r.Context())
			if !errors.Is(err, sql.ErrNoRows) && err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if err == nil {
				var snapshot repositoryreport.Snapshot
				if err := json.Unmarshal(data, &snapshot); err != nil {
					writeErr(w, http.StatusInternalServerError, err)
					return
				}
				result.Report.GeneratedAt = snapshot.GeneratedAt
				result.Report.RemoteEnabled = snapshot.RemoteEnabled
				result.Report.RemoteGeneratedAt = snapshot.RemoteGeneratedAt
				result.Report.Summary = snapshot.Summary
				result.Report.Comparison = snapshot.Comparison
			}
		}
		writeJSON(w, http.StatusOK, result)
	}
}
