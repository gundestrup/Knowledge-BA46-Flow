package analyzer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/websearch"
)

// fakeSearcher records queries and returns canned hits — the verify stage
// never touches the network in tests.
type fakeSearcher struct {
	queries []string
	results []websearch.Result
	err     error
}

func (f *fakeSearcher) Search(_ context.Context, query string, _ int) ([]websearch.Result, error) {
	f.queries = append(f.queries, query)
	return f.results, f.err
}

func TestFreshnessVerifyJudgesFlagsAgainstSearchEvidence(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntitle: curl\ntags: [tool, curl]\n---\n# curl\n\nThe latest curl release is 8.4.0 and supports HTTP/3.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{results: []websearch.Result{
		{Title: "curl changelog", URL: "https://curl.se/changes.html", Content: "curl 8.12.0 released February 2026"},
	}}
	llm := &fakeLLM{queue: []string{
		// Pass 1: heuristic flag, blind suggestion.
		`{"current":false,"summary":"Version pin may be stale.","flags":[{"claim":"latest curl release is 8.4.0","concern":"release claim, not verified"}],"suggestions":[]}`,
		// Pass 2: evidence-backed verdict and dated correction.
		`{"flags":[{"claim":"latest curl release is 8.4.0","verdict":"stale","evidence":"curl 8.12.0 is the latest per the official changelog","source":"https://curl.se/changes.html"}],
		  "suggestions":[{"action":"expand","target_path":"","reason":"8.4.0 is superseded by 8.12.0 per https://curl.se/changes.html","confidence":0.9,
		  "proposed_content":"---\ntitle: curl\ntags: [tool, curl]\n---\n# curl\n\nThe latest curl release is 8.4.0 and supports HTTP/3.\n\nAs of 2026, the latest release is 8.12.0 (https://curl.se/changes.html).\n"}]}`,
	}}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm, Verify: llm, Search: searcher}, config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || r.Suggestions != 1 {
		t.Fatalf("expected one verified check with one suggestion: %+v", r)
	}
	if len(llm.prompts) != 2 {
		t.Fatalf("verify should add a second generation call: %d prompts", len(llm.prompts))
	}
	if !strings.Contains(llm.prompts[1], "curl 8.12.0 released February 2026") {
		t.Fatal("search evidence did not reach the verify prompt")
	}
	if len(searcher.queries) != 1 || !strings.Contains(searcher.queries[0], "latest curl release is 8.4.0") {
		t.Fatalf("search query should carry the flagged claim: %v", searcher.queries)
	}

	sugs, err := db.Suggestions(ctx, "pending")
	if err != nil || len(sugs) != 1 {
		t.Fatalf("pending: %v %v", sugs, err)
	}
	if sugs[0].Origin != "freshness" || !strings.Contains(sugs[0].Reason, "curl.se") {
		t.Fatalf("verify suggestion should carry its evidence: %+v", sugs[0])
	}

	// kbm.freshness records the verified verdict, evidence and source.
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "tools/curl.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"verified: true", "verdict: stale", "curl.se/changes.html", "verdict: review"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("kbm.freshness missing %q:\n%s", want, raw)
		}
	}
}

func TestFreshnessVerifyPromptChangeDoesNotRepeatBaseFreshnessPass(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	verifyPromptPath := filepath.Join(c.PromptDirectory, "freshness-verify.md")
	if err := os.WriteFile(verifyPromptPath, []byte("VERIFY-TASK-v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntitle: curl\ntags: [tool, curl]\n---\n# curl\n\nThe latest curl release is 8.4.0.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{results: []websearch.Result{
		{Title: "curl changelog", URL: "https://curl.se/changes.html", Content: "curl 8.12.0 released"},
	}}
	firstLLM := &fakeLLM{queue: []string{
		`{"current":false,"summary":"Version pin may be stale.","flags":[{"claim":"latest curl release is 8.4.0","concern":"release claim"}],"suggestions":[]}`,
		`{"flags":[{"claim":"latest curl release is 8.4.0","verdict":"stale","evidence":"curl 8.12.0 is current","source":"https://curl.se/changes.html"}],"suggestions":[]}`,
	}}
	first, err := RunTask(ctx, c, db, Generators{Analysis: firstLLM, Verify: firstLLM, Search: searcher},
		config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil || first.Processed != 1 || len(firstLLM.prompts) != 2 {
		t.Fatalf("first freshness + verification=%+v err=%v calls=%d", first, err, len(firstLLM.prompts))
	}
	baseStamp := docByPath(t, db, "tools/curl.md").FreshnessSHA256
	if err := os.WriteFile(verifyPromptPath, []byte("VERIFY-TASK-v2"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}

	verifyOnlyLLM := &fakeLLM{reply: `{"flags":[{"claim":"latest curl release is 8.4.0","verdict":"current","evidence":"verified against v2 prompt","source":"https://curl.se/changes.html"}],"suggestions":[]}`}
	second, err := RunTask(ctx, c, db, Generators{Analysis: verifyOnlyLLM, Verify: verifyOnlyLLM, Search: searcher},
		config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil || second.Processed != 1 || len(verifyOnlyLLM.prompts) != 1 {
		t.Fatalf("verify-only pass=%+v err=%v calls=%d", second, err, len(verifyOnlyLLM.prompts))
	}
	if !strings.Contains(verifyOnlyLLM.prompts[0], "VERIFY-TASK-v2") ||
		strings.Contains(verifyOnlyLLM.prompts[0], "FRESHNESS") {
		t.Fatalf("expected only the verifier prompt after its edit: %s", verifyOnlyLLM.prompts[0])
	}
	doc := docByPath(t, db, "tools/curl.md")
	if doc.FreshnessSHA256 != baseStamp {
		t.Fatalf("verification-only refresh changed the base freshness stamp: %q -> %q", baseStamp, doc.FreshnessSHA256)
	}
	if doc.FreshnessVerifyPromptSHA == "" {
		t.Fatal("verification-only pass did not stamp the verifier prompt")
	}
}

func TestFreshnessVerifyRequiresSearchClient(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if _, err := RunTask(ctx, c, db, Generators{Analysis: &fakeLLM{}}, config.TaskFreshness, RunOptions{WebVerify: true}); err == nil {
		t.Fatal("web-verify without a search client must fail")
	}
}

func TestFreshnessVerifyPromptIsRequiredOnlyForOptIn(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/ok.md",
		"---\ntags: [tool]\n---\n# ok\n\nNothing versioned here.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	baseLLM := &fakeLLM{reply: `{"current":true,"summary":"ok","flags":[],"suggestions":[]}`}
	if _, err := RunTask(ctx, c, db, Generators{Analysis: baseLLM}, config.TaskFreshness, RunOptions{}); err != nil {
		t.Fatalf("base freshness should work without the opt-in verifier prompt: %v", err)
	}
	verifyLLM := &fakeLLM{}
	if _, err := RunTask(ctx, c, db,
		Generators{Analysis: verifyLLM, Verify: verifyLLM, Search: &fakeSearcher{}},
		config.TaskFreshness, RunOptions{WebVerify: true}); err == nil ||
		!strings.Contains(err.Error(), "freshness-verify.md") {
		t.Fatalf("opt-in web verification should require its prompt: %v", err)
	}
}

func TestFreshnessVerifyFailureDegradesToUnverified(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntags: [tool]\n---\n# curl\n\nThe latest curl release is 8.4.0.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	// Search hard-fails on the flag: the verify pass still runs, and the
	// honest answer is unverifiable.
	searcher := &fakeSearcher{err: errors.New("api key rejected")}
	llm := &fakeLLM{queue: []string{
		`{"current":false,"summary":"s","flags":[{"claim":"latest curl release is 8.4.0","concern":"c"}],"suggestions":[]}`,
		`{"flags":[{"claim":"latest curl release is 8.4.0","verdict":"unverifiable","evidence":"no evidence gathered","source":""}],"suggestions":[]}`,
	}}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm, Verify: llm, Search: searcher}, config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 {
		t.Fatalf("a search failure must not fail the document: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "tools/curl.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"verified: true", "unverifiable", "verdict: review"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("unverifiable flag should keep the doc in review:\n%s", raw)
		}
	}
}

func TestFreshnessVerifyBrokenJSONKeepsPassOneResult(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntags: [tool]\n---\n# curl\n\nThe latest curl release is 8.4.0.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{results: []websearch.Result{
		{Title: "t", URL: "https://example.com", Content: "c"},
	}}
	bad := `{"flags":[{"claim": ` // malformed — retried once, still broken
	llm := &fakeLLM{queue: []string{
		`{"current":false,"summary":"s","flags":[{"claim":"latest curl release is 8.4.0","concern":"c"}],
		  "suggestions":[{"action":"expand","target_path":"","reason":"blind guess","confidence":0.5,
		  "proposed_content":"---\ntags: [tool]\n---\n# curl\n\nThe latest curl release is 8.4.0.\n\nUpdated.\n"}]}`,
		bad, bad,
	}}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm, Verify: llm, Search: searcher}, config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || r.Suggestions != 1 {
		t.Fatalf("broken verify output should degrade to the pass-1 result: %+v", r)
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "tools/curl.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "verified: true") || !strings.Contains(string(raw), "verify_error") {
		t.Fatalf("failed verification must be recorded, not claimed:\n%s", raw)
	}
}

func TestFreshnessVerifySkipsWhenNoFlags(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/ok.md",
		"---\ntags: [tool]\n---\n# ok\n\nNothing versioned here.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{}
	llm := &fakeLLM{reply: `{"current":true,"summary":"ok","flags":[],"suggestions":[]}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm, Verify: llm, Search: searcher}, config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 || len(searcher.queries) != 0 || len(llm.prompts) != 1 {
		t.Fatalf("no flags means no search and no second pass: %+v queries=%v", r, searcher.queries)
	}
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK-v2"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	noCall := &fakeLLM{}
	again, err := RunTask(ctx, c, db, Generators{Analysis: noCall, Verify: noCall, Search: searcher},
		config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil || again.Skipped != 1 || len(noCall.prompts) != 0 || len(searcher.queries) != 0 {
		t.Fatalf("verify prompt change with no saved flags should only advance lineage: result=%+v err=%v prompts=%v queries=%v",
			again, err, noCall.prompts, searcher.queries)
	}
}

func TestFreshnessVerifyKeepsPrivateDocsOffline(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "private.md",
		"---\ntags: [tool]\nkb_policy: {no_llm_context: true}\n---\n# P\n\nPrivate claims about versions.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{}
	llm := &fakeLLM{reply: `{"current":false,"summary":"s","flags":[{"claim":"x","concern":"y"}],"suggestions":[]}`}
	r, err := RunTask(ctx, c, db, Generators{Analysis: llm, Verify: llm, Search: searcher}, config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Skipped != 1 || len(llm.prompts) != 0 || len(searcher.queries) != 0 {
		t.Fatalf("no_llm_context must never reach generation or search: %+v", r)
	}
}

func TestFreshnessVerifyRequiresVerifyClient(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	// Search present but no verify generator — the second pass has nothing to
	// re-judge evidence with, so the run must fail fast.
	if _, err := RunTask(ctx, c, db, Generators{Analysis: &fakeLLM{}, Search: &fakeSearcher{}},
		config.TaskFreshness, RunOptions{WebVerify: true}); err == nil {
		t.Fatal("web-verify without a verify generation client must fail")
	}
}

func TestFreshnessVerifyUsesDedicatedVerifyModel(t *testing.T) {
	ctx := context.Background()
	c, db := taskSetup(t)
	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("VERIFY-TASK"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal(err)
	}
	writeDoc(t, c.RepositoryPath, "tools/curl.md",
		"---\ntags: [tool]\n---\n# curl\n\nThe latest curl release is 8.4.0.\n")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	// The verification pass routes through its own task lane: a configured
	// ollama_task_models.verify default, recorded as verify_model.
	c.OllamaTaskModels = map[string][]string{config.TaskVerify: {"verify-model"}}
	flagPass := &fakeLLM{reply: `{"current":false,"summary":"s","flags":[{"claim":"latest curl release is 8.4.0","concern":"c"}],"suggestions":[]}`}
	verifyPass := &fakeLLM{reply: `{"flags":[{"claim":"latest curl release is 8.4.0","verdict":"stale","evidence":"e","source":"https://curl.se"}],"suggestions":[]}`}
	searcher := &fakeSearcher{results: []websearch.Result{
		{Title: "t", URL: "https://curl.se", Content: "c"},
	}}
	r, err := RunTask(ctx, c, db, Generators{Analysis: flagPass, Verify: verifyPass, Search: searcher},
		config.TaskFreshness, RunOptions{WebVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Processed != 1 {
		t.Fatalf("expected one verified check: %+v", r)
	}
	if len(flagPass.prompts) != 1 || len(verifyPass.prompts) != 1 {
		t.Fatalf("flag pass must use the analysis client and verify its own: flag=%d verify=%d",
			len(flagPass.prompts), len(verifyPass.prompts))
	}
	if !strings.Contains(verifyPass.prompts[0], "search results") {
		t.Fatal("verify client should receive the evidence prompt")
	}
	raw, err := os.ReadFile(filepath.Join(c.RepositoryPath, "tools/curl.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"verified: true", "verify_model: verify-model"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("kbm.freshness missing %q:\n%s", want, raw)
		}
	}
}
