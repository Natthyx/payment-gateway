// Package audit writes immutable records of sensitive operations.
//
// Every API key rotation, refund, merchant deactivation, manual ledger adjustment
// and webhook outcome should produce one audit_log row. The table is enforced
// append-only at the DB level (see migrations/004_audit_log.sql) so a compromised
// app process cannot rewrite history.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

type Service struct {
	db *pgxpool.Pool
}

func NewService(db *pgxpool.Pool) *Service {
	return &Service{db: db}
}

type Entry struct {
	MerchantID *int64
	Actor      string
	Action     string
	Target     string
	IP         string
	UserAgent  string
	RequestID  string
	Details    map[string]interface{}
}

// Record writes an audit row. Failure to audit must never block the business
// operation — we log loudly and continue. Compliance reviewers compare audit
// gaps against application logs to catch any drift.
func (s *Service) Record(ctx context.Context, e Entry) {
	var detailsJSON []byte
	if e.Details != nil {
		if b, err := json.Marshal(e.Details); err == nil {
			detailsJSON = b
		}
	}
	var ip interface{}
	if v := normalizeIP(e.IP); v != "" {
		ip = v
	}
	_, err := s.db.Exec(ctx, `
        INSERT INTO audit_log
            (merchant_id, actor, action, target, ip, user_agent, request_id, details)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
    `, e.MerchantID, e.Actor, e.Action, e.Target, ip, e.UserAgent, e.RequestID, detailsJSON)
	if err != nil {
		log.Error().Err(err).
			Str("actor", e.Actor).
			Str("action", e.Action).
			Msg("audit write failed")
	}
}

// normalizeIP turns r.RemoteAddr-style "host:port" into a bare address the
// INET column accepts, and drops anything that is not an IP so a malformed
// value can never make the audit insert fail.
func normalizeIP(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	if net.ParseIP(s) == nil {
		return ""
	}
	return s
}

// MerchantActor builds the canonical actor string for a merchant.
func MerchantActor(id int64) string { return fmt.Sprintf("merchant:%d", id) }
