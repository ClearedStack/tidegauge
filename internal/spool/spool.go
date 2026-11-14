// Package spool manages Tidegauge's local staging directory (/var/spool/cs) so
// completed watch outcomes survive Muster restarts on a read-only root
// filesystem.
package spool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ClearedStack/tidegauge/internal/muster"
)

// DefaultDir is the writable emptyDir mount path in deploy/tidegauge.yaml.
const DefaultDir = "/var/spool/cs"

// Call kinds staged in the outbox.
const (
	CallComplete     = "complete"
	CallFail         = "fail"
	CallNeedsSubtool = "needs_subtool"
)

// Entry is one deterministic worker outcome staged before calling Muster.
type Entry struct {
	Seq    uint64          `json:"seq"`
	ItemID string          `json:"item_id"`
	Lease  string          `json:"lease"`
	Call   string          `json:"call"`
	State  string          `json:"state,omitempty"`
	Code   string          `json:"code,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Queue persists outbound Muster calls in FIFO order under dir/outbox and
// provides dir/tmp for bounded scratch files during feed parsing.
type Queue struct {
	mu     sync.Mutex
	outbox string
	tmp    string
	seq    uint64
}

// New initializes the spool directory layout and resumes sequence numbering
// past any staged files left from an earlier run.
func New(dir string) (*Queue, error) {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	outbox := filepath.Join(dir, "outbox")
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		return nil, fmt.Errorf("spool: cannot create outbox directory")
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("spool: cannot create scratch directory")
	}

	// Clean up incomplete .tmp files from an interrupted write or fetch.
	if leftovers, err := os.ReadDir(outbox); err == nil {
		for _, d := range leftovers {
			if strings.HasSuffix(d.Name(), ".tmp") {
				_ = os.Remove(filepath.Join(outbox, d.Name()))
			}
		}
	}
	if scratch, err := os.ReadDir(tmp); err == nil {
		for _, d := range scratch {
			_ = os.Remove(filepath.Join(tmp, d.Name()))
		}
	}

	var maxSeq uint64
	entries, err := os.ReadDir(outbox)
	if err != nil {
		return nil, fmt.Errorf("spool: cannot read outbox directory")
	}
	for _, d := range entries {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			continue
		}
		prefix, _, ok := strings.Cut(d.Name(), "-")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(prefix, 10, 64); err == nil && n > maxSeq {
			maxSeq = n
		}
	}

	return &Queue{
		outbox: outbox,
		tmp:    tmp,
		seq:    maxSeq,
	}, nil
}

// TempDir returns the writable scratch directory inside the spool mount.
func (q *Queue) TempDir() string { return q.tmp }

// Pending returns the number of unacknowledged entries currently in the outbox.
func (q *Queue) Pending() (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	files, err := q.listLocked()
	return len(files), err
}

// Enqueue atomically writes entry to disk before any Muster delivery attempt.
func (q *Queue) Enqueue(entry Entry) (string, error) {
	if entry.Lease == "" || entry.Call == "" {
		return "", fmt.Errorf("spool: lease and call are required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	q.seq++
	entry.Seq = q.seq
	data, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("spool: cannot encode entry")
	}
	if len(data) > muster.MaxBodyBytes {
		return "", fmt.Errorf("spool: entry exceeds 64 KiB contract limit")
	}

	name := fmt.Sprintf("%016d-%s.json", entry.Seq, safeToken(entry.Lease))
	finalPath := filepath.Join(q.outbox, name)
	tmpFile, err := os.CreateTemp(q.outbox, "stage-*.tmp")
	if err != nil {
		return "", fmt.Errorf("spool: cannot stage entry")
	}
	tmpPath := tmpFile.Name()
	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmpFile.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("spool: cannot write staged entry")
	}
	if err := tmpFile.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("spool: cannot sync staged entry")
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("spool: cannot close staged entry")
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("spool: cannot commit staged entry")
	}
	return finalPath, nil
}

// Flush delivers all staged entries in FIFO order. Retryable errors leave the
// entry in the outbox and return immediately; terminal contract refusals log a
// sanitized notice and discard the rejected entry so the queue does not wedge.
func (q *Queue) Flush(ctx context.Context, client *muster.Client, logf func(format string, args ...any)) error {
	q.mu.Lock()
	files, err := q.listLocked()
	q.mu.Unlock()
	if err != nil {
		return err
	}

	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("spool: cannot read staged entry")
		}
		var entry Entry
		if err := json.Unmarshal(raw, &entry); err != nil {
			if logf != nil {
				logf("spool: dropping corrupt staged entry")
			}
			_ = os.Remove(path)
			continue
		}

		callErr := deliver(ctx, client, entry)
		if callErr == nil {
			_ = os.Remove(path)
			continue
		}
		if muster.IsRetryable(callErr) {
			return callErr
		}

		// Terminal refusal (for example 409 lease_not_held or 409 move_not_allowed):
		// Muster definitively refused this transition, so repeating it would fail forever.
		if logf != nil {
			if re, ok := muster.UnwrapRefusal(callErr); ok {
				logf("spool: dropping refused %s for %s (status=%d code=%s cause=%s)",
					entry.Call, safeToken(entry.ItemID), re.Status, re.Refusal.Error, re.Cause)
			} else {
				logf("spool: dropping non-retryable %s for %s", entry.Call, safeToken(entry.ItemID))
			}
		}
		_ = os.Remove(path)
	}
	return nil
}

func deliver(ctx context.Context, client *muster.Client, e Entry) error {
	switch e.Call {
	case CallComplete:
		var res any
		if len(e.Result) > 0 {
			res = e.Result
		}
		_, err := client.Complete(ctx, e.Lease, e.State, res)
		return err
	case CallFail:
		_, err := client.Fail(ctx, e.Lease, e.Code)
		return err
	case CallNeedsSubtool:
		_, err := client.NeedsSubtool(ctx, e.Lease)
		return err
	default:
		return &muster.ClientError{Message: "unknown spooled call"}
	}
}

func (q *Queue) listLocked() ([]string, error) {
	entries, err := os.ReadDir(q.outbox)
	if err != nil {
		return nil, fmt.Errorf("spool: cannot list outbox")
	}
	var names []string
	for _, d := range entries {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".json") {
			names = append(names, filepath.Join(q.outbox, d.Name()))
		}
	}
	sort.Strings(names)
	return names, nil
}

func safeToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 48; i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "item"
	}
	return b.String()
}
