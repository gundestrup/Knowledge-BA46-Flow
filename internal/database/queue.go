package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const queueSchema = `
CREATE TABLE IF NOT EXISTS review_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 suggestion_id INTEGER NOT NULL,
 batch_id TEXT NOT NULL,
 actor TEXT NOT NULL,
 classification TEXT NOT NULL,
 previous_status TEXT NOT NULL,
 new_status TEXT NOT NULL,
 reason TEXT NOT NULL,
 current_sha256 TEXT NOT NULL DEFAULT '',
 previous_decided_at TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_review_events_suggestion ON review_events(suggestion_id,id);
`

type queueReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ActiveSuggestions includes missing documents: reconciliation must not hide
// the very stale rows it is intended to explain and retire.
func (d *DB) ActiveSuggestions(ctx context.Context) ([]Suggestion, error) {
	return activeSuggestions(ctx, d)
}

func activeSuggestions(ctx context.Context, reader queueReader) ([]Suggestion, error) {
	rows, err := reader.QueryContext(ctx, `SELECT id,document_path,source_sha256,action,target_path,reason,
confidence,proposed_content,status,origin,created_at,decision_note,error,COALESCE((SELECT lower(json_extract(d.frontmatter_json,'$.document_id')) FROM documents d WHERE d.path=suggestions.document_path AND json_valid(d.frontmatter_json)),'')
FROM suggestions WHERE status IN ('pending','accepted') ORDER BY document_path,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suggestion{}
	for rows.Next() {
		var s Suggestion
		if err := rows.Scan(&s.ID, &s.DocumentPath, &s.SourceSHA256, &s.Action, &s.TargetPath,
			&s.Reason, &s.Confidence, &s.ProposedContent, &s.Status, &s.Origin,
			&s.CreatedAt, &s.DecisionNote, &s.Error, &s.DocumentID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func QueueVersion(suggestions []Suggestion) string {
	raw, _ := json.Marshal(suggestions)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

type Retirement struct {
	ID             int64
	Classification string
	Reason         string
	CurrentSHA256  string
}

// RetireQueue is all-or-nothing and refuses any changed queue. Machine
// retirements never populate decision_note, which is human training feedback.
func (d *DB) RetireQueue(ctx context.Context, version string, retirements []Retirement) (string, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	current, err := activeSuggestions(ctx, tx)
	if err != nil {
		return "", err
	}
	if QueueVersion(current) != version {
		return "", fmt.Errorf("review queue changed; preview reconciliation again")
	}
	statuses := map[int64]string{}
	for _, s := range current {
		statuses[s.ID] = s.Status
	}
	batch := rand.Text()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for _, item := range retirements {
		switch item.Classification {
		case "invalid", "stale", "missing":
		default:
			return "", fmt.Errorf("cannot retire %s proposals", item.Classification)
		}
		if statuses[item.ID] == "" {
			return "", fmt.Errorf("suggestion #%d is no longer active", item.ID)
		}
		reason := "queue reconciliation: " + item.Reason
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_events
(suggestion_id,batch_id,actor,classification,previous_status,new_status,reason,current_sha256,previous_decided_at,created_at)
SELECT id,?,'system',?,status,'superseded',?,?,COALESCE(decided_at,''),? FROM suggestions WHERE id=?`,
			batch, item.Classification, reason, item.CurrentSHA256, at, item.ID); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET status='superseded',error=?,decided_at=COALESCE(decided_at,?) WHERE id=?`,
			reason, at, item.ID); err != nil {
			return "", err
		}
	}
	return batch, tx.Commit()
}

func (d *DB) WithdrawAccepted(ctx context.Context, id int64) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO review_events
(suggestion_id,batch_id,actor,classification,previous_status,new_status,reason,previous_decided_at,created_at)
SELECT id,?,'human','withdrawal','accepted','pending','acceptance withdrawn',COALESCE(decided_at,''),?
FROM suggestions WHERE id=? AND status='accepted'`, rand.Text(), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("suggestion #%d is not accepted", id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE suggestions SET status='pending',decided_at=NULL,error='' WHERE id=? AND status='accepted'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveIdentityProposal cannot invalidate an existing review queue. It creates
// a pending, version-bound proposal; it never writes a Markdown file.
func (d *DB) SaveIdentityProposal(ctx context.Context, path, hash, proposed string) error {
	result, err := d.ExecContext(ctx, `INSERT INTO suggestions
(document_path,source_sha256,action,reason,confidence,proposed_content,origin,status,created_at)
SELECT path,sha256,'expand','Initialize an immutable document UUID (frontmatter only)',1,?,'identity','pending',?
FROM documents WHERE path=? AND sha256=? AND status<>'missing'
AND NOT EXISTS (SELECT 1 FROM suggestions WHERE document_path=? AND status IN ('pending','accepted'))`,
		proposed, time.Now().UTC().Format(time.RFC3339Nano), path, hash, path)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("note or review queue changed; finish its existing proposals and scan again")
	}
	return nil
}
