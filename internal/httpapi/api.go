// Package httpapi exposes the Phase 2 TokenLedger HTTP API.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tokenledger/internal/database/postgres"
	"tokenledger/internal/domain"
	"tokenledger/internal/ledger"
)

type API struct {
	service *ledger.Service
	token   string
	health  func(context.Context) error
}

func New(service *ledger.Service, token string, health func(context.Context) error) http.Handler {
	return &API{service: service, token: token, health: health}
}
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if err := a.health(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "internal_error", "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if value == "" || subtle.ConstantTimeCompare([]byte(value), []byte(a.token)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authorization required")
		return
	}
	a.route(w, r)
}
func (a *API) route(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "owners" {
		a.createOwner(w, r)
		return
	}
	if r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "register-sources" {
		a.registerSource(w, r)
		return
	}
	if len(parts) >= 3 && parts[1] == "owners" {
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || id <= 0 {
			writeError(w, 400, "validation_error", "owner ID must be positive")
			return
		}
		if len(parts) == 4 {
			switch parts[3] {
			case "deposits":
				if r.Method == http.MethodPost {
					a.deposit(w, r, id)
					return
				}
			case "spends":
				if r.Method == http.MethodPost {
					a.spend(w, r, id)
					return
				}
			case "adjustments":
				if r.Method == http.MethodPost {
					a.adjust(w, r, id)
					return
				}
			case "balance":
				if r.Method == http.MethodGet {
					a.balance(w, r, id)
					return
				}
			case "transactions":
				if r.Method == http.MethodGet {
					a.list(w, r, id)
					return
				}
			case "reconcile":
				if r.Method == http.MethodPost {
					a.reconcile(w, r, id)
					return
				}
			}
		}
	}
	if r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "transactions" {
		id, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			writeError(w, 400, "validation_error", "transaction ID must be positive")
			return
		}
		a.transaction(w, r, id)
		return
	}
	writeError(w, 404, "not_found", "not found")
}

type ownerRequest struct {
	Type        domain.OwnerType `json:"type"`
	ExternalRef string           `json:"external_ref"`
	DisplayName string           `json:"display_name"`
	Metadata    domain.Metadata  `json:"metadata"`
}

type sourceRequest struct {
	Name        string          `json:"name"`
	DisplayName string          `json:"display_name"`
	Metadata    domain.Metadata `json:"metadata"`
}

func (a *API) registerSource(w http.ResponseWriter, r *http.Request) {
	var request sourceRequest
	if !decode(w, r, &request) {
		return
	}
	account, err := a.service.RegisterSource(r.Context(), ledger.RegisterSourceCommand{Name: request.Name, DisplayName: request.DisplayName, Metadata: request.Metadata})
	if err != nil {
		respondError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, account)
}

func (a *API) createOwner(w http.ResponseWriter, r *http.Request) {
	var request ownerRequest
	if !decode(w, r, &request) {
		return
	}
	owner, err := a.service.CreateOwner(r.Context(), ledger.CreateOwnerCommand{Type: request.Type, ExternalRef: request.ExternalRef, DisplayName: request.DisplayName, Metadata: request.Metadata})
	if err != nil {
		respondError(w, err)
		return
	}
	writeJSON(w, 201, owner)
}

type moneyRequest struct {
	Amount         domain.Amount   `json:"amount"`
	Description    string          `json:"description"`
	ExternalSource string          `json:"external_source"`
	ExternalID     string          `json:"external_id"`
	Metadata       domain.Metadata `json:"metadata"`
}

func (q moneyRequest) command() (ledger.DepositCommand, error) {
	key := domain.IdempotencyKey{Source: q.ExternalSource, ID: q.ExternalID}
	return ledger.DepositCommand{Amount: q.Amount, Description: q.Description, Key: key, Metadata: q.Metadata}, key.Validate()
}
func (a *API) deposit(w http.ResponseWriter, r *http.Request, id int64) {
	var q moneyRequest
	if !decode(w, r, &q) {
		return
	}
	c, e := q.command()
	if e != nil {
		respondError(w, e)
		return
	}
	c.OwnerID = id
	out, e := a.service.Deposit(r.Context(), c)
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 201, out)
}
func (a *API) spend(w http.ResponseWriter, r *http.Request, id int64) {
	var q moneyRequest
	if !decode(w, r, &q) {
		return
	}
	c, e := q.command()
	if e != nil {
		respondError(w, e)
		return
	}
	c.OwnerID = id
	out, e := a.service.Spend(r.Context(), ledger.SpendCommand(c))
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 201, out)
}

type adjustmentRequest struct {
	Description    string           `json:"description"`
	ExternalSource string           `json:"external_source"`
	ExternalID     string           `json:"external_id"`
	Metadata       domain.Metadata  `json:"metadata"`
	Postings       []domain.Posting `json:"postings"`
}

func (a *API) adjust(w http.ResponseWriter, r *http.Request, id int64) {
	var q adjustmentRequest
	if !decode(w, r, &q) {
		return
	}
	out, e := a.service.Adjust(r.Context(), ledger.AdjustCommand{OwnerID: id, Description: q.Description, Key: domain.IdempotencyKey{Source: q.ExternalSource, ID: q.ExternalID}, Metadata: q.Metadata, Postings: q.Postings})
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 201, out)
}
func (a *API) balance(w http.ResponseWriter, r *http.Request, id int64) {
	amount, e := a.service.GetBalance(r.Context(), id)
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 200, map[string]int64{"owner_id": id, "available_balance": amount})
}
func (a *API) reconcile(w http.ResponseWriter, r *http.Request, id int64) {
	old, amount, repaired, e := a.service.ReconcileOwner(r.Context(), id)
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"owner_id": id, "previous_available_balance": old, "available_balance": amount, "repaired": repaired})
}
func (a *API) transaction(w http.ResponseWriter, r *http.Request, id int64) {
	out, e := a.service.GetTransaction(r.Context(), id)
	if e != nil {
		respondError(w, e)
		return
	}
	writeJSON(w, 200, out)
}

type cursorData struct {
	CreatedAt time.Time `json:"created_at"`
	ID        int64     `json:"id"`
}

func (a *API) list(w http.ResponseWriter, r *http.Request, id int64) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 100 {
			writeError(w, 400, "validation_error", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	cursor, e := decodeCursor(r.URL.Query().Get("cursor"))
	if e != nil {
		writeError(w, 400, "validation_error", "invalid cursor")
		return
	}
	page, e := a.service.ListTransactions(r.Context(), id, limit, cursor)
	if e != nil {
		respondError(w, e)
		return
	}
	next := ""
	if page.Next != nil {
		next = encodeCursor(*page.Next)
	}
	writeJSON(w, 200, map[string]any{"items": page.Items, "next_cursor": next})
}
func encodeCursor(c postgres.PageCursor) string {
	b, _ := json.Marshal(cursorData{c.CreatedAt, c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(raw string) (*postgres.PageCursor, error) {
	if raw == "" {
		return nil, nil
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil {
		return nil, e
	}
	var c cursorData
	if e = json.Unmarshal(b, &c); e != nil || c.ID <= 0 || c.CreatedAt.IsZero() {
		return nil, fmt.Errorf("bad cursor")
	}
	return &postgres.PageCursor{CreatedAt: c.CreatedAt, ID: c.ID}, nil
}
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, 400, "validation_error", "invalid JSON request")
		return false
	}
	return true
}
func respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrOwnerNotFound), errors.Is(err, domain.ErrTransactionNotFound), errors.Is(err, domain.ErrAccountNotFound):
		writeError(w, 404, "not_found", err.Error())
	case errors.Is(err, domain.ErrInsufficientFunds):
		writeError(w, 409, "insufficient_funds", "insufficient funds")
	case errors.Is(err, domain.ErrDuplicateTransaction):
		writeError(w, 409, "duplicate_transaction", "duplicate transaction")
	case errors.Is(err, domain.ErrInvalidAmount), errors.Is(err, domain.ErrInvalidPosting), errors.Is(err, domain.ErrInvalidMetadata), errors.Is(err, domain.ErrImbalancedTransaction), errors.Is(err, domain.ErrInvalidTransaction), errors.Is(err, domain.ErrInvalidIdempotencyKey), errors.Is(err, domain.ErrInvalidOwner), errors.Is(err, domain.ErrDuplicateOwner), errors.Is(err, domain.ErrDuplicateAccount):
		writeError(w, 400, "validation_error", err.Error())
	default:
		writeError(w, 500, "internal_error", "internal server error")
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
