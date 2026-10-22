package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/saief1/opengate/internal/secure"
)

// CreateKeyWithSecret inserts a virtual key with a caller-chosen plaintext
// (the demo traffic generator derives its keys deterministically so restarts
// reuse them instead of piling up new keys). Only the hash is stored.
func (s *Store) CreateKeyWithSecret(ctx context.Context, projectID, name, agent, user, plain string) (*VirtualKey, error) {
	if !secure.LooksLikeVirtualKey(plain) {
		return nil, fmt.Errorf("not a virtual key")
	}
	if _, err := s.GetProject(ctx, projectID); err != nil {
		return nil, fmt.Errorf("project %s: %w", projectID, err)
	}
	k := &VirtualKey{ID: secure.RandomID("key_"), ProjectID: projectID, Name: strings.TrimSpace(name),
		Agent: strings.TrimSpace(agent), UserLabel: strings.TrimSpace(user), Prefix: secure.DisplayPrefix(plain), CreatedAt: time.Now().UTC(),
		ModelPolicy: ModelPolicy{AllowedModels: []string{}, DeniedModels: []string{}}}
	_, err := s.exec(ctx, `INSERT INTO virtual_keys(id,project_id,name,agent,user_label,key_hash,prefix,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		k.ID, k.ProjectID, k.Name, k.Agent, k.UserLabel, secure.HashKey(plain), k.Prefix, ms(k.CreatedAt))
	if err != nil {
		return nil, err
	}
	return k, nil
}

// CountMCPBlocked counts an org's MCP calls refused by policy, rate limit or
// kill switch since t.
func (s *Store) CountMCPBlocked(ctx context.Context, orgID string, since time.Time) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM mcp_calls WHERE org_id=? AND ts>=? AND error_code IN ('tool_not_allowed','rate_limit_exceeded','key_killed','project_killed','killed')`,
		orgID, ms(since)).Scan(&n)
	return n, err
}

// PruneKeyRows deletes request logs and MCP calls of the given keys that are
// older than before, then keeps at most maxRows of each (newest first).
// It returns the number of rows deleted.
func (s *Store) PruneKeyRows(ctx context.Context, keyIDs []string, before time.Time, maxRows int) (int64, error) {
	if len(keyIDs) == 0 {
		return 0, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(keyIDs)), ",")
	args := make([]any, 0, len(keyIDs)+1)
	for _, k := range keyIDs {
		args = append(args, k)
	}
	var total int64
	for _, tbl := range []string{"request_logs", "mcp_calls"} {
		res, err := s.exec(ctx, `DELETE FROM `+tbl+` WHERE key_id IN (`+in+`) AND ts < ?`, append(append([]any{}, args...), ms(before))...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if maxRows <= 0 {
			continue
		}
		var cut int64
		err = s.queryRow(ctx, `SELECT ts FROM `+tbl+` WHERE key_id IN (`+in+`) ORDER BY ts DESC LIMIT 1 OFFSET ?`, append(append([]any{}, args...), maxRows)...).Scan(&cut)
		if err != nil {
			continue // fewer than maxRows rows (sql.ErrNoRows)
		}
		res, err = s.exec(ctx, `DELETE FROM `+tbl+` WHERE key_id IN (`+in+`) AND ts <= ?`, append(append([]any{}, args...), cut)...)
		if err != nil {
			return total, err
		}
		n, _ = res.RowsAffected()
		total += n
	}
	return total, nil
}
