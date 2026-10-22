package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDocumentLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "one", Size: 1, ModifiedUnix: 1, TagsJSON: `["a"]`}
	changed, err := db.UpsertScanned(ctx, doc, "scan1")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "A", "Summary", []string{"a", "tag"}, []Suggestion{{Action: "move", TargetPath: "b.md", Reason: "better", Confidence: .9}}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Pending(ctx, "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending=%v", pending)
	}
	suggestions, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions=%v", suggestions)
	}
	if pending, err := db.Pending(ctx, "other-model", "other-model", false); err != nil || len(pending) != 0 {
		t.Fatalf("model switch alone should not requeue: pending=%v err=%v", pending, err)
	}
	if pending, err := db.Pending(ctx, "other-model", "other-model", true); err != nil || len(pending) != 1 {
		t.Fatalf("reanalyze should include model drift: pending=%v err=%v", pending, err)
	}
	if err := db.SaveAnalysis(ctx, doc, "other-model", "A2", "Summary", []string{"a"}, []Suggestion{{Action: "expand", Reason: "other model view", Confidence: .5}}); err != nil {
		t.Fatal(err)
	}
	suggestions, err = db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 || suggestions[0].Action != "expand" {
		t.Fatalf("re-analysis should supersede prior pending suggestions: %v", suggestions)
	}
	if prior, err := db.Suggestions(ctx, "superseded"); err != nil || len(prior) != 1 || prior[0].Action != "move" {
		t.Fatalf("old suggestion should be superseded: %v err=%v", prior, err)
	}
	if pending, err := db.Pending(ctx, "other-model", "other-model", true); err != nil || len(pending) != 0 {
		t.Fatalf("same model should stay analyzed: pending=%v err=%v", pending, err)
	}
	if forced, err := db.Pending(ctx, "other-model", "other-model", false, true); err != nil || len(forced) != 1 {
		t.Fatalf("force-all should explicitly requeue stable documents: pending=%v err=%v", forced, err)
	}
	var model string
	if err := db.QueryRowContext(ctx, `SELECT analyzed_model FROM documents WHERE path='a.md'`).Scan(&model); err != nil || model != "other-model" {
		t.Fatalf("analyzed_model=%q err=%v", model, err)
	}
	if err := db.Decide(ctx, suggestions[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	accepted, err := db.Accepted(ctx)
	if err != nil || len(accepted) != 1 {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
	doc.SHA256 = "two"
	changed, err = db.UpsertScanned(ctx, doc, "scan2")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	pending, err = db.Pending(ctx, "test-model", "test-model", false)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	superseded, err := db.Suggestions(ctx, "superseded")
	if err != nil || len(superseded) != 2 {
		t.Fatalf("superseded=%+v err=%v", superseded, err)
	}
}

func TestChangedDocumentSupersedesPendingSuggestion(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doc := Document{Path: "guide.md", SHA256: "first", Size: 5, ModifiedUnix: 10, TagsJSON: `["guide"]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "Guide", "Summary", []string{"guide"}, []Suggestion{
		{Action: "expand", Reason: "add detail", ProposedContent: "# Guide\n\nMore"},
	}); err != nil {
		t.Fatal(err)
	}

	doc.SHA256 = "second"
	changed, err := db.UpsertScanned(ctx, doc, "scan-2")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("stale suggestions remain pending: %+v", pending)
	}
	superseded, err := db.Suggestions(ctx, "superseded")
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 || superseded[0].Status != "superseded" {
		t.Fatalf("superseded=%+v", superseded)
	}
}

func TestDocumentsAndSuggestionLookup(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "one.md", SHA256: "hash", Size: 4, ModifiedUnix: 3, TagsJSON: `["one"]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "One", "A summary", []string{"one"}, []Suggestion{
		{Action: "move", TargetPath: "two.md", Reason: "better location"},
	}); err != nil {
		t.Fatal(err)
	}

	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Title != "One" || docs[0].Path != "one.md" {
		t.Fatalf("documents=%+v", docs)
	}
	suggestions, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("suggestions=%+v", suggestions)
	}
	got, err := db.Suggestion(ctx, suggestions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetPath != "two.md" || got.ProposedContent != "" {
		t.Fatalf("suggestion=%+v", got)
	}
	if _, err := db.Suggestion(ctx, 999); err == nil {
		t.Fatal("expected missing suggestion error")
	}
	if err := db.Decide(ctx, 999, "accepted", ""); err == nil {
		t.Fatal("expected deciding missing suggestion to fail")
	}
	if err := db.Decide(ctx, suggestions[0].ID, "invalid-status", ""); err == nil {
		t.Fatal("expected invalid status to fail")
	}
}

func TestJobLifecycleAndStaleRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job, err := db.CreateJob(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "running" || job.ID == 0 {
		t.Fatalf("created job=%+v", job)
	}
	if err := db.FinishJob(ctx, job.ID, "done", "scanned 2", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.Detail != "scanned 2" || got.Finished == "" {
		t.Fatalf("finished job=%+v", got)
	}
	jobs, err := db.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatalf("jobs=%+v", jobs)
	}

	stale, err := db.CreateJob(ctx, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FailStaleJobs(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = db.Job(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error != "interrupted by server restart" || got.Finished == "" {
		t.Fatalf("stale job=%+v", got)
	}
}

func TestRunningJobSweepsStaleRunners(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, running, err := db.RunningJob(ctx); err != nil || running {
		t.Fatalf("empty db: running=%v err=%v", running, err)
	}

	live, err := db.CreateJob(ctx, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	got, running, err := db.RunningJob(ctx)
	if err != nil || !running || got.ID != live.ID {
		t.Fatalf("live job: running=%v got=%+v err=%v", running, got, err)
	}

	// A fresh detail update keeps the job alive; a frozen heartbeat dies.
	if err := db.UpdateJobDetail(ctx, live.ID, "analyzing 3/10: x.md"); err != nil {
		t.Fatal(err)
	}
	if _, running, err := db.RunningJob(ctx); err != nil || !running {
		t.Fatalf("after detail update: running=%v err=%v", running, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET heartbeat_at='2000-01-01T00:00:00Z' WHERE id=?`, live.ID); err != nil {
		t.Fatal(err)
	}
	if _, running, err := db.RunningJob(ctx); err != nil || running {
		t.Fatalf("stale runner should be swept: running=%v err=%v", running, err)
	}
	got, err = db.Job(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || got.Error != "interrupted (stale heartbeat)" {
		t.Fatalf("swept job=%+v", got)
	}
}

func TestUpdateJobDetailOnlyTouchesRunningJobs(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	running, err := db.CreateJob(ctx, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateJobDetail(ctx, running.ID, "analyzing 1/5: a.md"); err != nil {
		t.Fatal(err)
	}
	got, err := db.Job(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" || got.Detail != "analyzing 1/5: a.md" {
		t.Fatalf("running job=%+v", got)
	}

	if err := db.FinishJob(ctx, running.ID, "done", "final summary", ""); err != nil {
		t.Fatal(err)
	}
	// Progress updates must not resurrect or overwrite a finished job.
	if err := db.UpdateJobDetail(ctx, running.ID, "stale progress"); err != nil {
		t.Fatal(err)
	}
	got, err = db.Job(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.Detail != "final summary" {
		t.Fatalf("finished job=%+v", got)
	}
}

func TestReportSnapshotAndJobFiltering(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.CreateJob(ctx, "scan"); err != nil {
		t.Fatal(err)
	}
	reportJob, err := db.CreateJob(ctx, "report")
	if err != nil {
		t.Fatal(err)
	}
	regularJobs, err := db.JobsForOps(ctx, 100, "scan", "analyze", "run")
	if err != nil {
		t.Fatal(err)
	}
	reportJobs, err := db.JobsForOps(ctx, 100, "report")
	if err != nil {
		t.Fatal(err)
	}
	if len(regularJobs) != 1 || regularJobs[0].Op != "scan" {
		t.Fatalf("regular jobs=%+v", regularJobs)
	}
	if len(reportJobs) != 1 || reportJobs[0].ID != reportJob.ID {
		t.Fatalf("report jobs=%+v", reportJobs)
	}

	data := []byte(`{"configured":true,"repos":[]}`)
	if err := db.SaveRepoReportSnapshot(ctx, "2026-10-17T12:00:00Z", data); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRepoReportSnapshot(ctx, "2026-10-17T12:01:00Z", []byte(`{"configured":true,"repos":[{"name":"one"}]}`)); err != nil {
		t.Fatal(err)
	}
	generatedAt, got, err := db.RepoReportSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if generatedAt != "2026-10-17T12:01:00Z" || string(got) != `{"configured":true,"repos":[{"name":"one"}]}` {
		t.Fatalf("snapshot time=%q data=%s", generatedAt, got)
	}
	if err := db.SaveRepoReportSnapshot(ctx, "invalid", []byte("{")); err == nil {
		t.Fatal("invalid report JSON should be rejected")
	}
}

func TestKnowledgeDashboardCountsUnanalyzedFilesAndLastRunCycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	overview, err := db.KnowledgeDashboard(ctx, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if overview.ChangedFiles != 0 || overview.LastCycleAt != "" {
		t.Fatalf("initial overview=%+v", overview)
	}
	doc := Document{Path: "changed.md", SHA256: "new-hash", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan-1"); err != nil {
		t.Fatal(err)
	}
	overview, err = db.KnowledgeDashboard(ctx, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if overview.ChangedFiles != 1 {
		t.Fatalf("changed file count=%d", overview.ChangedFiles)
	}

	scan, err := db.CreateJob(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, scan.ID, "done", "scan", ""); err != nil {
		t.Fatal(err)
	}
	overview, err = db.KnowledgeDashboard(ctx, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if overview.LastCycleAt != "" {
		t.Fatalf("scan-only job counted as a full cycle: %+v", overview)
	}

	run, err := db.CreateJob(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJob(ctx, run.ID, "done", "cycle", ""); err != nil {
		t.Fatal(err)
	}
	latest, err := db.Job(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	overview, err = db.KnowledgeDashboard(ctx, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if overview.LastCycleAt != latest.Finished {
		t.Fatalf("last cycle=%q want %q", overview.LastCycleAt, latest.Finished)
	}
}

func TestLegacyJobsTableMigrationPreservesHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE jobs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 op TEXT NOT NULL CHECK(op IN ('scan','analyze','run')),
 status TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','done','failed')),
 detail TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 finished_at TEXT
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO jobs(id,op,status,detail,error,created_at,finished_at)
VALUES(7,'scan','done','previous job','','2026-10-01T12:00:00Z','2026-10-01T12:00:01Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	previous, err := db.Job(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if previous.Op != "scan" || previous.Detail != "previous job" || previous.Status != "done" {
		t.Fatalf("previous job after migration=%+v", previous)
	}
	reportJob, err := db.CreateJob(ctx, "report")
	if err != nil {
		t.Fatal(err)
	}
	if reportJob.Op != "report" || reportJob.ID != 8 {
		t.Fatalf("created job=%+v", reportJob)
	}
}

func TestCandidatePathsSaveErrorAndMarkMissing(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, doc := range []Document{
		{Path: "a.md", SHA256: "a", Size: 1, TagsJSON: "[]"},
		{Path: "b.md", SHA256: "b", Size: 1, TagsJSON: "[]"},
	} {
		if _, err := db.UpsertScanned(ctx, doc, "scan-1"); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := db.CandidatePaths(ctx, "a.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "b.md" {
		t.Fatalf("candidate paths=%v", paths)
	}
	if err := db.SaveError(ctx, "a.md", "model unavailable"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Pending(ctx, "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].LastError != "model unavailable" {
		t.Fatalf("pending=%+v", pending)
	}
	if err := db.MarkMissing(ctx, "scan-2"); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Status != "missing" || docs[1].Status != "missing" {
		t.Fatalf("documents=%+v", docs)
	}
}

func TestMarkAppliedAndApplyFailed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, doc := range []Document{
		{Path: "applied.md", SHA256: "one", Size: 1, TagsJSON: "[]"},
		{Path: "failed.md", SHA256: "two", Size: 1, TagsJSON: "[]"},
	} {
		if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveAnalysis(ctx, doc, "test-model", doc.Path, "", nil, []Suggestion{
			{Action: "delete", Reason: "obsolete"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, suggestion := range pending {
		if err := db.Decide(ctx, suggestion.ID, "accepted", ""); err != nil {
			t.Fatal(err)
		}
		if suggestion.DocumentPath == "applied.md" {
			if err := db.MarkApplied(ctx, suggestion.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := db.MarkApplyFailed(ctx, suggestion.ID, "filesystem error"); err != nil {
				t.Fatal(err)
			}
		}
	}
	applied, err := db.Suggestions(ctx, "applied")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := db.Suggestions(ctx, "failed")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || len(failed) != 1 {
		t.Fatalf("applied=%+v failed=%+v", applied, failed)
	}
}

func TestMarkSupersededAcceptedSuggestion(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "stale.md", SHA256: "source-hash", Size: 5, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "Stale", "", nil, []Suggestion{
		{Action: "expand", ProposedContent: "# Stale"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := db.Decide(ctx, pending[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkSuperseded(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.Suggestion(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "superseded" {
		t.Fatalf("status=%q", got.Status)
	}
}

func TestOpenRejectsUnusablePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// The database path's parent is a regular file — MkdirAll must fail.
	if _, err := Open(t.Context(), filepath.Join(file, "km.db")); err == nil {
		t.Fatal("expected Open to fail inside a regular file")
	}
}

func TestSaveAnalysisDropsNoOpSuggestions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "h", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "A", "", nil, []Suggestion{
		{Action: "none", Reason: "looks fine"},
		{Action: "expand", Reason: "add detail", ProposedContent: "# A\n\nMore"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Action != "expand" {
		t.Fatalf("'none' suggestion was stored: %+v", pending)
	}
}

func TestUnchangedScanPreservesAnalysisState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "h", Size: 1, TagsJSON: `["a"]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "A", "summary", []string{"a"}, nil); err != nil {
		t.Fatal(err)
	}
	changed, err := db.UpsertScanned(ctx, doc, "scan-2")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("an unchanged file must not be reported as changed")
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Status != "analyzed" {
		t.Fatalf("unchanged rescan reset status: %+v", docs)
	}
}

func TestJobsAreNewestFirstAndHonourLimit(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, op := range []string{"scan", "analyze", "run"} {
		if _, err := db.CreateJob(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := db.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || jobs[0].Op != "run" || jobs[2].Op != "scan" {
		t.Fatalf("jobs not newest-first: %+v", jobs)
	}
	limited, err := db.Jobs(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[1].Op != "analyze" {
		t.Fatalf("limited jobs=%+v", limited)
	}
	if _, err := db.Job(ctx, 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows for a missing job, got %v", err)
	}
}

func TestPruneFinishedJobsKeepsNewestAndSparesRunning(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 5; i++ {
		j, err := db.CreateJob(ctx, "scan")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishJob(ctx, j.ID, "done", "ok", ""); err != nil {
			t.Fatal(err)
		}
	}
	running, err := db.CreateJob(ctx, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := db.PruneFinishedJobs(ctx, 2)
	if err != nil || removed != 3 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	jobs, err := db.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || jobs[0].ID != running.ID {
		t.Fatalf("jobs=%+v", jobs)
	}
	// Pruning again with the same bound is a no-op, and keep=0 clears all
	// finished rows while still sparing the running job.
	if removed, err := db.PruneFinishedJobs(ctx, 2); err != nil || removed != 0 {
		t.Fatalf("re-prune removed=%d err=%v", removed, err)
	}
	if removed, err := db.PruneFinishedJobs(ctx, 0); err != nil || removed != 2 {
		t.Fatalf("keep=0 removed=%d err=%v", removed, err)
	}
	jobs, _ = db.Jobs(ctx, 10)
	if len(jobs) != 1 || jobs[0].Status != "running" {
		t.Fatalf("running job pruned: %+v", jobs)
	}
}

func TestCandidatePathsExcludesSelfAndMissing(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertScanned(ctx, Document{Path: "gone.md", SHA256: "a", Size: 1, TagsJSON: "[]"}, "scan-1"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"self.md", "other.md"} {
		if _, err := db.UpsertScanned(ctx, Document{Path: p, SHA256: "b", Size: 1, TagsJSON: "[]"}, "scan-2"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MarkMissing(ctx, "scan-2"); err != nil {
		t.Fatal(err)
	}
	paths, err := db.CandidatePaths(ctx, "self.md", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "other.md" {
		t.Fatalf("candidates=%v", paths)
	}
}

func TestDeletingDocumentCascadesSuggestions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "h", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "A", "", nil, []Suggestion{
		{Action: "delete", Reason: "obsolete"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM documents WHERE path=?`, "a.md"); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("suggestions survived a document delete: %+v", pending)
	}
}

func TestRepoReportSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, _, err := db.RepoReportSnapshot(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows before the first snapshot, got %v", err)
	}
	if err := db.SaveRepoReportSnapshot(ctx, "t1", []byte("not json")); err == nil {
		t.Fatal("expected invalid JSON to be rejected")
	}
	if err := db.SaveRepoReportSnapshot(ctx, "t1", []byte(`{"configured":true}`)); err != nil {
		t.Fatal(err)
	}
	at, data, err := db.RepoReportSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if at != "t1" || string(data) != `{"configured":true}` {
		t.Fatalf("snapshot at=%q data=%s", at, data)
	}
	// A second save replaces the single-row snapshot.
	if err := db.SaveRepoReportSnapshot(ctx, "t2", []byte(`{"configured":true,"repos":[]}`)); err != nil {
		t.Fatal(err)
	}
	at, data, err = db.RepoReportSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if at != "t2" || string(data) != `{"configured":true,"repos":[]}` {
		t.Fatalf("snapshot at=%q data=%s", at, data)
	}
}

func TestRepoReportHistoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.SaveRepoReportHistory(ctx, "t0", []byte("not json")); err == nil {
		t.Fatal("expected invalid JSON to be rejected")
	}
	summaries := []string{`{"problems":1}`, `{"problems":2}`, `{"problems":3}`}
	for i, summary := range summaries {
		if err := db.SaveRepoReportHistory(ctx, fmt.Sprintf("t%d", i), []byte(summary)); err != nil {
			t.Fatal(err)
		}
	}
	points, err := db.RepoReportHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || points[0].GeneratedAt != "t0" || points[2].GeneratedAt != "t2" {
		t.Fatalf("points=%+v", points)
	}
	if string(points[1].Summary) != `{"problems":2}` {
		t.Fatalf("summary=%s", points[1].Summary)
	}
	limited, err := db.RepoReportHistory(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].GeneratedAt != "t1" || limited[1].GeneratedAt != "t2" {
		t.Fatalf("limited=%+v", limited)
	}
}

func TestRepoReportHistoryPrunesOldest(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := range reportHistoryKeep + 3 {
		if err := db.SaveRepoReportHistory(ctx, fmt.Sprintf("t%04d", i), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	points, err := db.RepoReportHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != reportHistoryKeep {
		t.Fatalf("expected %d points, got %d", reportHistoryKeep, len(points))
	}
	if points[0].GeneratedAt != fmt.Sprintf("t%04d", 3) {
		t.Fatalf("oldest kept point=%q, want t0003", points[0].GeneratedAt)
	}
}

func TestKnowledgeProgress(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insertDoc := func(path, firstSeen string, analyzed any) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO documents(path,sha256,size,modified_unix,first_seen_at,last_seen_at,analyzed_at) VALUES(?,?,?,?,?,?,?)`,
			path, "h-"+path, 1, 1, firstSeen, firstSeen, analyzed)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertDoc("a.md", "2026-10-01T08:00:00Z", "2026-10-01T09:00:00Z")
	insertDoc("b.md", "2026-10-01T10:00:00Z", "")
	insertDoc("c.md", "2026-10-02T08:00:00Z", nil)

	if _, err := db.ExecContext(ctx, `INSERT INTO suggestions(document_path,source_sha256,action,reason,created_at,decided_at) VALUES('a.md','h','move','r','2026-10-01T11:00:00Z','2026-10-02T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO suggestions(document_path,source_sha256,action,reason,created_at,decided_at,applied_at) VALUES('b.md','h','merge','r','2026-10-02T13:00:00Z','2026-10-02T14:00:00Z','2026-10-02T15:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	days, err := db.KnowledgeProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days=%+v", days)
	}
	first, second := days[0], days[1]
	if first.Date != "2026-10-01" || first.Notes != 2 || first.Analyzed != 1 || first.Suggestions != 1 || first.Handled != 0 || first.Applied != 0 {
		t.Fatalf("day1=%+v", first)
	}
	if second.Date != "2026-10-02" || second.Notes != 1 || second.Analyzed != 0 || second.Suggestions != 1 || second.Handled != 2 || second.Applied != 1 {
		t.Fatalf("day2=%+v", second)
	}
}

func TestJobsForOpsFiltersByOperation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, op := range []string{"scan", "report", "analyze"} {
		if _, err := db.CreateJob(ctx, op); err != nil {
			t.Fatal(err)
		}
	}
	opsJobs, err := db.JobsForOps(ctx, 10, "scan", "analyze", "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(opsJobs) != 2 || opsJobs[0].Op != "analyze" || opsJobs[1].Op != "scan" {
		t.Fatalf("ops jobs=%+v", opsJobs)
	}
	reportJobs, err := db.JobsForOps(ctx, 10, "report")
	if err != nil {
		t.Fatal(err)
	}
	if len(reportJobs) != 1 || reportJobs[0].Op != "report" {
		t.Fatalf("report jobs=%+v", reportJobs)
	}
	empty, err := db.JobsForOps(ctx, 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ops filter: jobs=%+v err=%v", empty, err)
	}
}

// Databases created before the report feature have a jobs table whose CHECK
// constraint rejects op='report'; Open must migrate it in place.
func TestJobsTableMigratesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "km.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE jobs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 op TEXT NOT NULL CHECK(op IN ('scan','analyze','run')),
 status TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','done','failed')),
 detail TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 finished_at TEXT
);
INSERT INTO jobs(op,status,detail,created_at) VALUES('scan','done','Scanned 3 markdown files','2026-01-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The preserved row keeps its id and detail.
	got, err := db.Job(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Op != "scan" || got.Detail != "Scanned 3 markdown files" {
		t.Fatalf("migrated job=%+v", got)
	}
	// The migrated table accepts the new 'report' op.
	if _, err := db.CreateJob(context.Background(), "report"); err != nil {
		t.Fatalf("report job rejected after migration: %v", err)
	}
}

func TestMarkSupersededLeavesRejectedSuggestionsAlone(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "h", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "A", "", nil, []Suggestion{
		{Action: "delete", Reason: "obsolete"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := db.Decide(ctx, pending[0].ID, "rejected", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkSuperseded(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.Suggestion(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "rejected" {
		t.Fatalf("status=%q", got.Status)
	}
}

func TestLegacyDocumentsTableGainsFrontmatterColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-docs.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A documents table as it existed before the frontmatter columns.
	_, err = raw.Exec(`CREATE TABLE documents (
 path TEXT PRIMARY KEY,
 sha256 TEXT NOT NULL,
 analyzed_sha256 TEXT NOT NULL DEFAULT '',
 size INTEGER NOT NULL,
 modified_unix INTEGER NOT NULL,
 title TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 tags_json TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL DEFAULT 'pending',
 last_error TEXT NOT NULL DEFAULT '',
 first_seen_at TEXT NOT NULL,
 last_seen_at TEXT NOT NULL,
 analyzed_at TEXT
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO documents(path,sha256,size,modified_unix,status,first_seen_at,last_seen_at)
VALUES('old.md','hash1',10,1,'analyzed','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Existing row kept; new columns populated by the next upsert.
	if _, err := db.UpsertScanned(ctx, Document{
		Path: "old.md", SHA256: "hash2", Size: 10, TagsJSON: `["a"]`,
		FrontmatterJSON: `{"title":"Old"}`, FrontmatterIssues: `["x"]`,
	}, "scan2"); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].FrontmatterJSON != `{"title":"Old"}` || docs[0].FrontmatterIssues != `["x"]` {
		t.Fatalf("docs=%+v", docs)
	}
	// Unchanged rescan preserves the stored frontmatter.
	if _, err := db.UpsertScanned(ctx, Document{
		Path: "old.md", SHA256: "hash2", Size: 10, TagsJSON: `["a"]`,
		FrontmatterJSON: `{"title":"New"}`, FrontmatterIssues: `["y"]`,
	}, "scan3"); err != nil {
		t.Fatal(err)
	}
	docs, _ = db.Documents(ctx)
	if docs[0].FrontmatterJSON != `{"title":"Old"}` {
		t.Fatalf("frontmatter overwritten on unchanged rescan: %q", docs[0].FrontmatterJSON)
	}
}

func TestInboundLinksFindsRelatedAndContradictedBy(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, doc := range []Document{
		{Path: "target.md", SHA256: "t", Size: 1},
		{Path: "links.md", SHA256: "a", Size: 1, FrontmatterJSON: `{"related":["target.md","other.md"]}`},
		{Path: "contradicts.md", SHA256: "b", Size: 1, FrontmatterJSON: `{"contradicted_by":"target.md"}`},
		{Path: "unrelated.md", SHA256: "c", Size: 1, FrontmatterJSON: `{"related":["elsewhere.md"]}`},
		{Path: "substring.md", SHA256: "d", Size: 1, FrontmatterJSON: `{"related":["target.md.bak"]}`},
	} {
		if _, err := db.UpsertScanned(ctx, doc, "s"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.InboundLinks(ctx, "target.md")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, ",")
	if len(got) != 2 || !strings.Contains(joined, "links.md") || !strings.Contains(joined, "contradicts.md") {
		t.Fatalf("inbound=%v", got)
	}
}

func TestExternalLaneKeepsLocalLineage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "one", Size: 1, ModifiedUnix: 1, TagsJSON: `["a"]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "local-model", "A", "S", []string{"a"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveExternalAnalysis(ctx, doc, "ext-model", "A", "S", []string{"a"}, []Suggestion{{Action: "expand", Reason: "external view", Confidence: .8}}); err != nil {
		t.Fatal(err)
	}
	// The external run must not disturb the local lane's pending view.
	if pending, err := db.Pending(ctx, "local-model", "local-model", false); err != nil || len(pending) != 0 {
		t.Fatalf("local pending after external run=%v err=%v", pending, err)
	}
	if pending, err := db.PendingExternal(ctx, "ext-model", "local-model", false, false); err != nil || len(pending) != 0 {
		t.Fatalf("external pending=%v err=%v", pending, err)
	}
	docs, err := db.Documents(ctx)
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	if docs[0].AnalyzedModel != "local-model" || docs[0].AnalyzedSHA256 != "one" {
		t.Fatalf("local lineage overwritten: %+v", docs[0])
	}
	if docs[0].ExternalModel != "ext-model" || docs[0].ExternalSHA256 != "one" {
		t.Fatalf("external markers not stamped: %+v", docs[0])
	}
	// Model drift is tracked per lane: a different external model only shows
	// up when an external reanalyze is requested, never in the local lane.
	if pending, err := db.Pending(ctx, "local-model", "local-model", true); err != nil || len(pending) != 0 {
		t.Fatalf("local reanalyze should ignore the external model: pending=%v err=%v", pending, err)
	}
	if pending, err := db.PendingExternal(ctx, "newer-ext", "local-model", false, true); err != nil || len(pending) != 1 {
		t.Fatalf("external drift should requeue: pending=%v err=%v", pending, err)
	}
	// A content change is pending in both lanes again.
	doc.SHA256 = "two"
	if _, err := db.UpsertScanned(ctx, doc, "scan2"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := db.Pending(ctx, "local-model", "local-model", false); len(pending) != 1 {
		t.Fatalf("changed doc should be locally pending: %v", pending)
	}
	if pending, _ := db.PendingExternal(ctx, "ext-model", "local-model", false, false); len(pending) != 1 {
		t.Fatalf("changed doc should be externally pending: %v", pending)
	}
}

func TestMigrationAddsExternalColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "km.db")
	// Build a version-4-era database: the documents table predates the
	// external lane columns and is stamped one schema behind.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE documents (
 path TEXT PRIMARY KEY,
 sha256 TEXT NOT NULL,
 analyzed_sha256 TEXT NOT NULL DEFAULT '',
 analyzed_model TEXT NOT NULL DEFAULT '',
 enriched_sha256 TEXT NOT NULL DEFAULT '',
 enriched_model TEXT NOT NULL DEFAULT '',
 sig BLOB,
 embedding BLOB,
 size INTEGER NOT NULL,
 modified_unix INTEGER NOT NULL,
 title TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 tags_json TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL DEFAULT 'pending',
 last_error TEXT NOT NULL DEFAULT '',
 first_seen_at TEXT NOT NULL,
 last_seen_at TEXT NOT NULL,
 analyzed_at TEXT,
 frontmatter_json TEXT NOT NULL DEFAULT '',
 frontmatter_issues TEXT NOT NULL DEFAULT '[]'
);
INSERT INTO documents (path,sha256,size,modified_unix,first_seen_at,last_seen_at,analyzed_sha256,analyzed_model)
 VALUES ('a.md','one',1,1,'t','t','one','old-model');
PRAGMA user_version = 4;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	docs, err := db.Documents(context.Background())
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	if docs[0].AnalyzedModel != "old-model" || docs[0].ExternalModel != "" || docs[0].ExternalSHA256 != "" {
		t.Fatalf("migrated doc: %+v", docs[0])
	}
	// An external run on a migrated row stamps the new columns.
	doc := docs[0]
	if err := db.SaveExternalAnalysis(context.Background(), doc, "ext-model", "A", "S", nil, nil); err != nil {
		t.Fatal(err)
	}
	docs, _ = db.Documents(context.Background())
	if docs[0].ExternalModel != "ext-model" {
		t.Fatalf("external model after migration: %+v", docs[0])
	}
}

func TestSchemaVersionStampAndDowngradeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "km.db")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	var stamped int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&stamped); err != nil || stamped != schemaVersion {
		t.Fatalf("user_version=%d err=%v, want %d", stamped, err, schemaVersion)
	}
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(t.Context(), path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema should be refused, got %v", err)
	}
}

// heartbeatAt reads the raw heartbeat_at column: it is deliberately not part of
// the exported Job struct, so tests assert it directly.
func heartbeatAt(t *testing.T, db *DB, id int64) string {
	t.Helper()
	var hb string
	if err := db.QueryRow(`SELECT heartbeat_at FROM jobs WHERE id=?`, id).Scan(&hb); err != nil {
		t.Fatal(err)
	}
	return hb
}

func TestHeartbeatJobRefreshesOnlyRunningJobs(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job, err := db.CreateJob(ctx, "analyze")
	if err != nil {
		t.Fatal(err)
	}
	old := "2000-01-01T00:00:00Z"
	if _, err := db.Exec(`UPDATE jobs SET heartbeat_at=? WHERE id=?`, old, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.HeartbeatJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if hb := heartbeatAt(t, db, job.ID); hb == old || hb == "" {
		t.Fatalf("heartbeat not refreshed: %q", hb)
	}
	if err := db.FinishJob(ctx, job.ID, "done", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE jobs SET heartbeat_at=? WHERE id=?`, old, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.HeartbeatJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if hb := heartbeatAt(t, db, job.ID); hb != old {
		t.Fatalf("finished job heartbeat changed to %q", hb)
	}
}

func TestRunHeartbeatTicksUntilStopped(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job, err := db.CreateJob(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}
	initial := heartbeatAt(t, db, job.ID)
	stop := db.runHeartbeat(ctx, job.ID, 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	updated := ""
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		if updated = heartbeatAt(t, db, job.ID); updated > initial {
			break
		}
	}
	stop()
	if updated == "" || updated <= initial {
		t.Fatalf("heartbeat did not advance: initial=%q got=%q", initial, updated)
	}
}

func TestDerivedStateUpdatesAndTaskStamps(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doc := Document{Path: "a.md", SHA256: "sha-a", BodySHA256: "body-a", Size: 1, ModifiedUnix: 1, TagsJSON: `[]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}

	// Derived-state setters update only their own column.
	if err := db.SetLinkIssues(ctx, "a.md", `["broken a -> b"]`); err != nil {
		t.Fatal(err)
	}
	if err := db.RefreshFrontmatter(ctx, "a.md", `{"title":"A"}`, "[]"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSignature(ctx, "a.md", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored := docs[0]
	if stored.LinkIssues != `["broken a -> b"]` {
		t.Errorf("LinkIssues=%q", stored.LinkIssues)
	}
	if stored.FrontmatterJSON != `{"title":"A"}` {
		t.Errorf("FrontmatterJSON=%q", stored.FrontmatterJSON)
	}
	if string(stored.Signature) != string([]byte{1, 2, 3}) {
		t.Errorf("Signature=%v", stored.Signature)
	}
	if stored.SHA256 != "sha-a" {
		t.Errorf("derived updates must not touch content hash, got %q", stored.SHA256)
	}

	// PendingEnrich: unstamped doc is pending; after SaveEnrichment it is not;
	// a different fast model re-pends it under reanalyze.
	pending, err := db.PendingEnrich(ctx, "fast-1", false)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unstamped doc should be enrich-pending: %v %v", pending, err)
	}
	if err := db.SaveEnrichment(ctx, doc, "fast-1", "A", "s", []string{"x"}, nil); err != nil {
		t.Fatal(err)
	}
	if pending, _ = db.PendingEnrich(ctx, "fast-1", false); len(pending) != 0 {
		t.Fatalf("stamped doc should not be pending: %v", pending)
	}
	if pending, _ = db.PendingEnrich(ctx, "fast-2", true); len(pending) != 1 {
		t.Fatalf("model change + reanalyze should re-pend: %v", pending)
	}
	if pending, _ = db.PendingEnrich(ctx, "fast-1", false, true); len(pending) != 1 {
		t.Fatalf("forceAll should return everything: %v", pending)
	}

	// SaveTask stamps the task's own lineage and owns its suggestions —
	// callers stamp Origin with the task name, as the analyzer does.
	if err := db.SaveTask(ctx, doc, "expand", "big-1", []Suggestion{
		{Action: "expand", Reason: "stub", Confidence: .8, ProposedContent: "more", Origin: "expand"},
	}); err != nil {
		t.Fatal(err)
	}
	docs, _ = db.Documents(ctx)
	if docs[0].ExpandSHA256 != "sha-a" || docs[0].ExpandModel != "big-1" {
		t.Errorf("expand stamp=%q/%q", docs[0].ExpandSHA256, docs[0].ExpandModel)
	}
	if pending, _ = db.PendingTask(ctx, "expand", "big-1", false); len(pending) != 0 {
		t.Fatalf("expand-stamped doc should not be pending: %v", pending)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	if len(sugs) != 1 || sugs[0].Origin != "expand" {
		t.Fatalf("task suggestions=%+v", sugs)
	}
	// A second run supersedes the task's own pending suggestions only.
	if err := db.SaveTask(ctx, doc, "expand", "big-1", nil); err != nil {
		t.Fatal(err)
	}
	if sugs, _ = db.PendingSuggestions(ctx); len(sugs) != 0 {
		t.Fatalf("old task suggestion should be superseded: %+v", sugs)
	}

	// StampTask advances lineage without suggestions — freshness tracks the
	// body hash, not the canonical hash.
	if err := db.StampTask(ctx, doc, "freshness", "big-1"); err != nil {
		t.Fatal(err)
	}
	docs, _ = db.Documents(ctx)
	if docs[0].FreshnessSHA256 != "body-a" {
		t.Errorf("freshness should stamp body hash, got %q", docs[0].FreshnessSHA256)
	}
	if pending, _ = db.PendingTask(ctx, "freshness", "big-1", false); len(pending) != 0 {
		t.Fatalf("freshness-stamped doc should not be pending: %v", pending)
	}
	if err := db.StampTask(ctx, doc, "bogus-task", "m"); err == nil {
		t.Error("unknown task should fail")
	}
	if taskCurrent(doc, "freshness") != "body-a" || taskCurrent(doc, "expand") != "sha-a" {
		t.Error("taskCurrent picks the task's current column")
	}
	noBody := Document{SHA256: "sha-only"}
	if taskCurrent(noBody, "freshness") != "sha-only" {
		t.Error("freshness falls back to canonical hash without a body hash")
	}
}

func TestRunHeartbeatPublicWrapper(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job, err := db.CreateJob(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}
	// The public wrapper ticks every 15s — too slow to observe in a test —
	// but it must return a working stop that terminates the goroutine.
	stop := db.RunHeartbeat(ctx, job.ID)
	stop()
}

func TestLegacySchemaMigrationPreservesRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The oldest supported documents schema: none of the frontmatter,
	// external/enrich lane, signature, embedding or task lineage columns.
	_, err = raw.Exec(`CREATE TABLE documents (
 path TEXT PRIMARY KEY,
 sha256 TEXT NOT NULL,
 analyzed_sha256 TEXT NOT NULL DEFAULT '',
 size INTEGER NOT NULL,
 modified_unix INTEGER NOT NULL,
 title TEXT NOT NULL DEFAULT '',
 summary TEXT NOT NULL DEFAULT '',
 tags_json TEXT NOT NULL DEFAULT '[]',
 status TEXT NOT NULL DEFAULT 'pending',
 last_error TEXT NOT NULL DEFAULT '',
 first_seen_at TEXT NOT NULL,
 last_seen_at TEXT NOT NULL,
 analyzed_at TEXT
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO documents(path,sha256,analyzed_sha256,size,modified_unix,title,status,first_seen_at,last_seen_at,analyzed_at)
VALUES('a.md','sha-a','sha-a',10,5,'Doc A','analyzed','2026-10-01T00:00:00Z','2026-10-01T00:00:00Z','2026-10-01T00:00:01Z')`)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy suggestions: pre-origin, pre-decision bookkeeping, narrow
	// origin CHECK that predates the task stages.
	_, err = raw.Exec(`CREATE TABLE suggestions (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('none','move','merge','expand','delete')),
 target_path TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL,
 confidence REAL NOT NULL DEFAULT 0,
 proposed_content TEXT NOT NULL DEFAULT '',
 origin TEXT NOT NULL DEFAULT 'analysis' CHECK(origin IN ('analysis','enrich','deterministic')),
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','rejected','applied','failed','superseded')),
 created_at TEXT NOT NULL,
 decided_at TEXT,
 applied_at TEXT,
 error TEXT NOT NULL DEFAULT '',
 FOREIGN KEY(document_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO suggestions(document_path,source_sha256,action,reason,confidence,origin,status,created_at)
VALUES('a.md','sha-a','move','tidy',0.5,'analysis','pending','2026-10-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy stage_results: same columns, CHECK without the task stages.
	_, err = raw.Exec(`CREATE TABLE stage_results (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 document_path TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 stage TEXT NOT NULL CHECK(stage IN ('enrich','analysis')),
 model TEXT NOT NULL,
 outcome TEXT NOT NULL CHECK(outcome IN ('success','error','skipped')),
 patch_outcome TEXT NOT NULL DEFAULT '',
 patch_json TEXT NOT NULL DEFAULT 'null',
 derived_fields_json TEXT NOT NULL DEFAULT '[]',
 metadata_json TEXT NOT NULL DEFAULT '{}',
 suggestions INTEGER NOT NULL DEFAULT 0,
 rejected_json TEXT NOT NULL DEFAULT '[]',
 error TEXT NOT NULL DEFAULT '',
 duration_ms INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,
 FOREIGN KEY(document_path) REFERENCES documents(path) ON UPDATE CASCADE ON DELETE CASCADE
)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO stage_results(document_path,source_sha256,stage,model,outcome,suggestions,created_at)
VALUES('a.md','sha-a','analysis','old-model','success',1,'2026-10-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Every pre-existing row survives with its values intact.
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "a.md" || docs[0].AnalyzedSHA256 != "sha-a" || docs[0].Title != "Doc A" {
		t.Fatalf("documents after migration=%+v", docs)
	}
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sugs) != 1 || sugs[0].Reason != "tidy" || sugs[0].Origin != "analysis" {
		t.Fatalf("suggestions after migration=%+v", sugs)
	}
	var stageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM stage_results`).Scan(&stageCount); err != nil {
		t.Fatal(err)
	}
	if stageCount != 1 {
		t.Fatalf("stage_results rows=%d", stageCount)
	}
	// The widened constraints now admit the task stages and origins.
	doc := Document{Path: "a.md", SHA256: "sha-a"}
	if err := db.SaveTask(ctx, doc, "expand", "m", nil); err != nil {
		t.Fatalf("expand task write after migration: %v", err)
	}
	if err := db.StampTask(ctx, doc, "freshness", "m"); err != nil {
		t.Fatalf("freshness stamp after migration: %v", err)
	}
}

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var current int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), path)
	if err == nil {
		db.Close()
		t.Fatal("database stamped newer than this build must not open")
	}
	if !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("error=%v", err)
	}
}

// Fault-injection tests: SQLite RAISE(ABORT) triggers fail a specific
// statement inside our transactions — the recovery contract is that the
// error surfaces and the transaction leaves no half-written state, the same
// class of failure a real SQLite error (disk full, constraint, I/O) causes.

func TestSaveTaskFailureLeavesNoPartialState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "sha-a", Size: 1, ModifiedUnix: 1, TagsJSON: `[]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}

	// Fail exactly the suggestion INSERT inside SaveTask's transaction.
	if _, err := db.Exec(`CREATE TRIGGER fail_suggestion_insert BEFORE INSERT ON suggestions
BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = db.SaveTask(ctx, doc, "expand", "m", []Suggestion{
		{Action: "expand", Reason: "x", Confidence: .5, Origin: "expand"},
	})
	if err == nil || !strings.Contains(err.Error(), "injected insert failure") {
		t.Fatalf("SaveTask error=%v", err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_suggestion_insert`); err != nil {
		t.Fatal(err)
	}

	// Recovery contract: the aborted tx must not leave the lineage stamp or
	// any suggestion behind — the doc stays expand-pending, not half-done.
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if docs[0].ExpandSHA256 != "" || docs[0].ExpandModel != "" {
		t.Fatalf("aborted SaveTask left lineage stamp: %+v", docs[0])
	}
	if sugs, _ := db.PendingSuggestions(ctx); len(sugs) != 0 {
		t.Fatalf("aborted SaveTask left suggestions: %+v", sugs)
	}
	if pending, _ := db.PendingTask(ctx, "expand", "m", false); len(pending) != 1 {
		t.Fatalf("doc should still be expand-pending: %v", pending)
	}
}

func TestUpsertScannedFailureLeavesNoPartialRow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Fail the documents write inside UpsertScanned's transaction.
	if _, err := db.Exec(`CREATE TRIGGER fail_doc_update BEFORE UPDATE ON documents
BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`); err != nil {
		t.Fatal(err)
	}
	doc := Document{Path: "a.md", SHA256: "sha-a", Size: 1, ModifiedUnix: 1, TagsJSON: `[]`}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	doc.SHA256 = "sha-b" // changed hash takes the UPDATE path
	_, err = db.UpsertScanned(ctx, doc, "scan2")
	if err == nil || !strings.Contains(err.Error(), "injected update failure") {
		t.Fatalf("UpsertScanned error=%v", err)
	}

	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The first write is intact; the failed one left nothing behind.
	if len(docs) != 1 || docs[0].SHA256 != "sha-a" {
		t.Fatalf("documents after failed upsert=%+v", docs)
	}
}

func TestOpenRejectsCorruptDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), path)
	if err == nil {
		db.Close()
		t.Fatal("corrupt database file must not open")
	}
}

// TestModelStampsReportsDominantModel pins the "what ran last time" contract
// the UI warns against: the most common recorded model per lineage column,
// with its document count, and no entries for columns nobody stamped.
func TestModelStampsReportsDominantModel(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	models := []string{"model-a", "model-a", "model-a", "model-b"}
	for i, model := range models {
		doc := Document{Path: fmt.Sprintf("d%d.md", i), SHA256: fmt.Sprintf("sha-%d", i), Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
		if _, err := db.UpsertScanned(ctx, doc, "s"); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveAnalysis(ctx, doc, model, "t", "s", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs[:2] {
		if err := db.SaveEmbedding(ctx, doc, "emb-x", []byte{0, 0, 128, 63}); err != nil {
			t.Fatal(err)
		}
	}
	// A minority re-stamp must not flip the dominant lineage answer.
	if err := db.SaveTask(ctx, docs[0], "freshness", "fresh-old", nil); err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs[1:] {
		if err := db.SaveTask(ctx, doc, "freshness", "fresh-new", nil); err != nil {
			t.Fatal(err)
		}
	}

	stamps, err := db.ModelStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stamps["analyzed"]; got.Model != "model-a" || got.Docs != 3 {
		t.Fatalf("analyzed stamp=%+v, want model-a over 3 docs", got)
	}
	if got := stamps["embedded"]; got.Model != "emb-x" || got.Docs != 2 {
		t.Fatalf("embedded stamp=%+v, want emb-x over 2 docs", got)
	}
	if got := stamps["freshness"]; got.Model != "fresh-new" || got.Docs != 3 {
		t.Fatalf("freshness stamp=%+v, want fresh-new over 3 docs", got)
	}
	for _, lane := range []string{"external", "enriched", "expand"} {
		if _, ok := stamps[lane]; ok {
			t.Fatalf("lane %q must stay absent when nothing recorded it: %+v", lane, stamps[lane])
		}
	}

	// Vanished files must not keep voting: once the model-a notes go missing,
	// the minority live model becomes the dominant lineage answer.
	if err := db.MarkMissing(ctx, "scan-later"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertScanned(ctx, Document{Path: "d3.md", SHA256: "sha-3", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}, "scan-later"); err != nil {
		t.Fatal(err)
	}
	stamps, err = db.ModelStamps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stamps["analyzed"]; got.Model != "model-b" || got.Docs != 1 {
		t.Fatalf("analyzed stamp after missing=%+v, want model-b over the live doc", got)
	}
}

// TestRejectedSuggestionSuppressionAndNote covers the review-feedback loop:
// a rejected proposal is never re-inserted on a later pass, its identity keys
// (action+target, normalized reason, content) are the matching rule, and the
// reviewer's note rides along for prompt context.
func TestRejectedSuggestionSuppressionAndNote(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "one", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "s"); err != nil {
		t.Fatal(err)
	}
	save := func(sugs ...Suggestion) {
		t.Helper()
		if err := db.SaveAnalysis(ctx, doc, "m", "t", "s", nil, sugs); err != nil {
			t.Fatal(err)
		}
	}
	pending := func() []Suggestion {
		t.Helper()
		sugs, err := db.PendingSuggestions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sugs
	}

	save(
		Suggestion{Action: "move", TargetPath: "b.md", Reason: "better place", Confidence: .9},
		Suggestion{Action: "expand", Reason: "PostgreSQL 16 is the latest release", ProposedContent: "# a\n\nnew body", Confidence: .8},
	)
	rows := pending()
	if len(rows) != 2 {
		t.Fatalf("pending=%v", rows)
	}
	if err := db.Decide(ctx, rows[0].ID, "rejected", "b.md exists on another shelf"); err != nil {
		t.Fatal(err)
	}
	if err := db.Decide(ctx, rows[1].ID, "rejected", "PG 18 was released Sep 2025 — claim is backwards"); err != nil {
		t.Fatal(err)
	}

	// The identical proposals must not come back — not even a rephrased
	// reason on a targeted action, nor the same expand content under a new
	// reason. A genuinely different proposal still lands.
	save(
		Suggestion{Action: "move", TargetPath: "b.md", Reason: "reorganize shelf", Confidence: .9},
		Suggestion{Action: "expand", Reason: "correct version claim", ProposedContent: "# a\n\nnew body", Confidence: .8},
		Suggestion{Action: "expand", Reason: "add a troubleshooting section", Confidence: .7},
		Suggestion{Action: "merge", TargetPath: "c.md", Reason: "duplicates", Confidence: .7},
	)
	rows = pending()
	if len(rows) != 2 {
		t.Fatalf("pending=%v, want the fresh expand + merge only", rows)
	}
	for _, s := range rows {
		if s.Reason == "add a troubleshooting section" || s.Action == "merge" {
			continue
		}
		t.Fatalf("suppressed proposal resurfaced: %+v", s)
	}

	// The reviewer's notes survive for prompt feedback, newest first.
	rejected, err := db.RejectedSuggestions(ctx, "a.md", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected) != 2 || rejected[0].DecisionNote == "" || rejected[1].DecisionNote == "" {
		t.Fatalf("rejected=%+v", rejected)
	}
	if rejected[0].Action != "expand" || !strings.Contains(rejected[0].DecisionNote, "PG 18") {
		t.Fatalf("newest rejected=%+v", rejected[0])
	}

	// A long note is bounded.
	if err := db.Decide(ctx, pending()[0].ID, "rejected", strings.Repeat("x", 900)); err != nil {
		t.Fatal(err)
	}
	rejected, _ = db.RejectedSuggestions(ctx, "a.md", 5)
	if got := len([]rune(rejected[0].DecisionNote)); got > 500 {
		t.Fatalf("decision_note length=%d", got)
	}
}

// TestMigrationAddsDecisionNote opens a v8-era database whose suggestions
// table predates reviewer notes: migrate must add the column so Decide can
// store feedback and RejectedSuggestions can return it.
func TestMigrationAddsDecisionNote(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "km.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the v8-era suggestions table (no decision_note) and stamp the
	// schema one version back so migrate runs the column upgrade.
	if _, err := raw.Exec(`ALTER TABLE suggestions DROP COLUMN decision_note`); err != nil {
		raw.Close()
		t.Skipf("sqlite build cannot DROP COLUMN: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 8`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	doc := Document{Path: "a.md", SHA256: "one", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "s"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "m", "t", "s", nil, []Suggestion{{Action: "expand", Reason: "old claim", Confidence: .8}}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PendingSuggestions(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending=%v err=%v", rows, err)
	}
	if err := db.Decide(ctx, rows[0].ID, "rejected", "not true anymore"); err != nil {
		t.Fatalf("Decide on migrated schema: %v", err)
	}
	rejected, err := db.RejectedSuggestions(ctx, "a.md", 5)
	if err != nil || len(rejected) != 1 || rejected[0].DecisionNote != "not true anymore" {
		t.Fatalf("rejected=%+v err=%v", rejected, err)
	}
}

// TestSuggestionsSkipMissingDocuments: a suggestion for a file the scanner no
// longer sees must leave the review and apply queues — but only while the
// file is gone. Rescanning a reappeared file restores its document status and
// the still-valid suggestions resurface with it.
func TestSuggestionsSkipMissingDocuments(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "one", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "m", "t", "s", nil, []Suggestion{
		{Action: "expand", Reason: "fix me", Confidence: .8},
		{Action: "move", TargetPath: "b.md", Reason: "better", Confidence: .8},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	if err := db.Decide(ctx, pending[1].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	// The next scan does not see a.md: both queues drop its suggestions.
	if err := db.MarkMissing(ctx, "scan-2"); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]func(context.Context) ([]Suggestion, error){
		"pending":  db.PendingSuggestions,
		"accepted": db.Accepted,
	} {
		if rows, err := list(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("%s for missing doc=%v err=%v", name, rows, err)
		}
	}
	// Direct id access still works — the rows are hidden, not deleted.
	if got, err := db.Suggestion(ctx, pending[0].ID); err != nil || got.Status != "pending" {
		t.Fatalf("row lookup=%+v err=%v", got, err)
	}

	// The file reappears unchanged: its document status is restored
	// (analyzed, since the hash still matches the stamp) and its
	// suggestions re-enter the queues.
	if _, err := db.UpsertScanned(ctx, doc, "scan-3"); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(ctx)
	if err != nil || len(docs) != 1 || docs[0].Status != "analyzed" {
		t.Fatalf("resurrected doc=%+v err=%v", docs, err)
	}
	if rows, err := db.PendingSuggestions(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("pending after resurrection=%v err=%v", rows, err)
	}
	if rows, err := db.Accepted(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("accepted after resurrection=%v err=%v", rows, err)
	}
}

// TestRenameByHashPreservesLineage: a moved file is the same entity — its
// canonical hash is its identity. Scan must re-key the row rather than treat
// the new path as a fresh document, so analysis stamps, embeddings, and
// review decisions (FK cascade) all survive the move.
func TestRenameByHashPreservesLineage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := Document{Path: "a.md", SHA256: "one", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "model-x", "T", "S", []string{"x"}, []Suggestion{
		{Action: "expand", Reason: "polish", Confidence: .8},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbedding(ctx, doc, "embed-m", []byte{0, 0, 128, 63}); err != nil {
		t.Fatal(err)
	}

	// The file moves to a new location with identical content.
	moved := doc
	moved.Path = "sub/b.md"
	changed, err := db.UpsertScanned(ctx, moved, "scan-2")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("rename must report unchanged — the entity did not change")
	}
	if err := db.MarkMissing(ctx, "scan-2"); err != nil {
		t.Fatal(err)
	}

	docs, err := db.Documents(ctx)
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
	got := docs[0]
	if got.Path != "sub/b.md" || got.Status != "analyzed" || got.AnalyzedModel != "model-x" {
		t.Fatalf("renamed row lost lineage: %+v", got)
	}
	if got.EmbeddedSHA256 != "one" || got.EmbeddedModel != "embed-m" || len(got.Embedding) == 0 {
		t.Fatalf("renamed row lost embedding: %+v", got)
	}
	// The pending suggestion followed the row — same entity, new path.
	rows, err := db.PendingSuggestions(ctx)
	if err != nil || len(rows) != 1 || rows[0].DocumentPath != "sub/b.md" {
		t.Fatalf("pending=%v err=%v", rows, err)
	}
	// And it is not queued for re-analysis.
	pendingDocs, err := db.Pending(ctx, "model-x", "model-x", false)
	if err != nil || len(pendingDocs) != 0 {
		t.Fatalf("moved doc should not be pending: %v err=%v", pendingDocs, err)
	}
}

// TestRenameByHashAmbiguousAndDuplicate: hash equality must only re-key when
// the match is unambiguous — two vanished same-hash files, or a live file at
// the old path, mean the new path is a separate document.
func TestRenameByHashAmbiguousAndDuplicate(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := func(path string) {
		t.Helper()
		if _, err := db.UpsertScanned(ctx, Document{Path: path, SHA256: "dup", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}, "scan-1"); err != nil {
			t.Fatal(err)
		}
	}
	seed("a1.md")
	seed("a2.md")

	// Both vanish; a new same-hash path is ambiguous — plain new document.
	if err := db.MarkMissing(ctx, "scan-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertScanned(ctx, Document{Path: "c.md", SHA256: "dup", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}, "scan-3"); err != nil {
		t.Fatal(err)
	}
	docs, _ := db.Documents(ctx)
	var cDoc Document
	count := 0
	for _, d := range docs {
		if d.Path == "c.md" {
			cDoc, count = d, count+1
		}
	}
	if count != 1 || cDoc.Status == "missing" {
		t.Fatalf("ambiguous same-hash insert=%+v", docs)
	}
	for _, d := range docs {
		if (d.Path == "a1.md" || d.Path == "a2.md") && d.Status != "missing" {
			t.Fatalf("vanished originals should stay missing: %+v", d)
		}
	}

	// A live same-hash sibling means the new path is a duplicate, not a move.
	if _, err := db.UpsertScanned(ctx, Document{Path: "d.md", SHA256: "dup", Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}, "scan-3"); err != nil {
		t.Fatal(err)
	}
	docs, _ = db.Documents(ctx)
	count = 0
	for _, d := range docs {
		if d.SHA256 == "dup" {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("live duplicate must not steal the row: %v", docs)
	}
}

// TestRetargetPath follows rename chains to the live endpoint and refuses
// dead ends — a link is only retargeted to a document that actually exists.
func TestRetargetPath(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := func(path, sha, seen string) {
		t.Helper()
		if _, err := db.UpsertScanned(ctx, Document{Path: path, SHA256: sha, Size: 1, ModifiedUnix: 1, TagsJSON: "[]"}, seen); err != nil {
			t.Fatal(err)
		}
	}

	// a.md → b.md → c.md across three scans (the file keeps moving).
	seed("a.md", "one", "s1")
	seed("b.md", "one", "s2")
	seed("c.md", "one", "s3")
	if got, ok := db.RetargetPath(ctx, "a.md"); !ok || got != "c.md" {
		t.Fatalf("a.md retarget=%q,%v want c.md,true", got, ok)
	}
	if got, ok := db.RetargetPath(ctx, "b.md"); !ok || got != "c.md" {
		t.Fatalf("b.md retarget=%q,%v want c.md,true", got, ok)
	}

	// A renamed-then-deleted file offers no retarget.
	seed("gone.md", "two", "s1")
	seed("gone-new.md", "two", "s4")
	if err := db.MarkMissing(ctx, "s5"); err != nil { // nothing seen at s5
		t.Fatal(err)
	}
	if got, ok := db.RetargetPath(ctx, "gone.md"); ok || got != "" {
		t.Fatalf("deleted endpoint must not retarget: %q,%v", got, ok)
	}

	// No rename recorded → no retarget.
	if got, ok := db.RetargetPath(ctx, "never.md"); ok || got != "" {
		t.Fatalf("unknown path retarget=%q,%v", got, ok)
	}
}
