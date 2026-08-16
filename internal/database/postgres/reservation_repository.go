package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"tokenledger/internal/domain"
)

// Reservation is the mutable settlement projection for one reserve journal transaction.
type Reservation struct {
	ReservationTransactionID int64         `json:"reservation_transaction_id"`
	OwnerID                  int64         `json:"owner_id"`
	OriginalAmount           domain.Amount `json:"original_amount"`
	CapturedAmount           domain.Amount `json:"captured_amount"`
	ReleasedAmount           domain.Amount `json:"released_amount"`
	Status                   string        `json:"status"`
	CreatedAt                time.Time     `json:"created_at"`
	UpdatedAt                time.Time     `json:"updated_at"`
}

type ReservationRepository struct{}

func (ReservationRepository) Create(ctx context.Context, tx *sql.Tx, reservationID, ownerID int64, amount domain.Amount) (Reservation, error) {
	var result Reservation
	err := tx.QueryRowContext(ctx, `INSERT INTO ledger_reservations (reservation_transaction_id, owner_id, original_amount)
		VALUES ($1, $2, $3) RETURNING reservation_transaction_id, owner_id, original_amount, captured_amount, released_amount, status, created_at, updated_at`, reservationID, ownerID, amount).
		Scan(&result.ReservationTransactionID, &result.OwnerID, &result.OriginalAmount, &result.CapturedAmount, &result.ReleasedAmount, &result.Status, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return result, fmt.Errorf("create reservation: %w", err)
	}
	return result, nil
}

func (ReservationRepository) Get(ctx context.Context, db singleRowQuerier, reservationID int64) (Reservation, error) {
	return scanReservation(db.QueryRowContext(ctx, `SELECT reservation_transaction_id, owner_id, original_amount, captured_amount, released_amount, status, created_at, updated_at FROM ledger_reservations WHERE reservation_transaction_id = $1`, reservationID))
}

func (ReservationRepository) GetForUpdate(ctx context.Context, tx *sql.Tx, reservationID int64) (Reservation, error) {
	return scanReservation(tx.QueryRowContext(ctx, `SELECT reservation_transaction_id, owner_id, original_amount, captured_amount, released_amount, status, created_at, updated_at FROM ledger_reservations WHERE reservation_transaction_id = $1 FOR UPDATE`, reservationID))
}

func (ReservationRepository) Settle(ctx context.Context, tx *sql.Tx, reservation Reservation, capture bool, amount domain.Amount) (Reservation, error) {
	if amount <= 0 || reservation.CapturedAmount+reservation.ReleasedAmount+amount > reservation.OriginalAmount {
		return Reservation{}, domain.ErrReservationOverSettled
	}
	column := "released_amount"
	if capture {
		column = "captured_amount"
	}
	query := fmt.Sprintf(`UPDATE ledger_reservations SET %s = %s + $2, status = CASE WHEN captured_amount + released_amount + $2 = original_amount THEN 'settled' ELSE 'open' END, updated_at = now()
		WHERE reservation_transaction_id = $1 RETURNING reservation_transaction_id, owner_id, original_amount, captured_amount, released_amount, status, created_at, updated_at`, column, column)
	return scanReservation(tx.QueryRowContext(ctx, query, reservation.ReservationTransactionID, amount))
}

func scanReservation(row *sql.Row) (Reservation, error) {
	var result Reservation
	err := row.Scan(&result.ReservationTransactionID, &result.OwnerID, &result.OriginalAmount, &result.CapturedAmount, &result.ReleasedAmount, &result.Status, &result.CreatedAt, &result.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("%w", domain.ErrReservationNotFound)
	}
	if err != nil {
		return result, fmt.Errorf("get reservation: %w", err)
	}
	return result, nil
}
