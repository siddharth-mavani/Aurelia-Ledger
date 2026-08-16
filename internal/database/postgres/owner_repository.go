package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"aurelialedger/internal/domain"
)

// OwnerRepository persists owners. Writes intentionally require the caller's
// transaction so a later posting can atomically update its projection.
type OwnerRepository struct{}

func (OwnerRepository) Create(ctx context.Context, tx *sql.Tx, owner domain.Owner) (domain.Owner, error) {
	if err := owner.Validate(); err != nil {
		return domain.Owner{}, err
	}
	metadata := normalizeMetadata(owner.Metadata)
	err := tx.QueryRowContext(ctx, `
		INSERT INTO ledger_owners (owner_type, external_ref, display_name, metadata)
		VALUES ($1, $2, $3, $4)
		RETURNING id, cached_balance, created_at, updated_at`,
		owner.Type, owner.ExternalRef, owner.DisplayName, metadata,
	).Scan(&owner.ID, &owner.CachedBalance, &owner.CreatedAt, &owner.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Owner{}, fmt.Errorf("%w: %s/%s", domain.ErrDuplicateOwner, owner.Type, owner.ExternalRef)
		}
		return domain.Owner{}, fmt.Errorf("insert owner: %w", err)
	}
	owner.Metadata = metadata
	return owner, nil
}

func (OwnerRepository) Get(ctx context.Context, db singleRowQuerier, id int64) (domain.Owner, error) {
	var owner domain.Owner
	err := db.QueryRowContext(ctx, `
		SELECT id, owner_type, external_ref, display_name, cached_balance, metadata, created_at, updated_at
		FROM ledger_owners WHERE id = $1`, id,
	).Scan(&owner.ID, &owner.Type, &owner.ExternalRef, &owner.DisplayName, &owner.CachedBalance, &owner.Metadata, &owner.CreatedAt, &owner.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Owner{}, fmt.Errorf("%w: %d", domain.ErrOwnerNotFound, id)
	}
	if err != nil {
		return domain.Owner{}, fmt.Errorf("get owner: %w", err)
	}
	return owner, nil
}

// GetForUpdate takes the owner row lock needed before changing cached_balance.
func (OwnerRepository) GetForUpdate(ctx context.Context, tx *sql.Tx, id int64) (domain.Owner, error) {
	var owner domain.Owner
	err := tx.QueryRowContext(ctx, `
		SELECT id, owner_type, external_ref, display_name, cached_balance, metadata, created_at, updated_at
		FROM ledger_owners WHERE id = $1 FOR UPDATE`, id,
	).Scan(&owner.ID, &owner.Type, &owner.ExternalRef, &owner.DisplayName, &owner.CachedBalance, &owner.Metadata, &owner.CreatedAt, &owner.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Owner{}, fmt.Errorf("%w: %d", domain.ErrOwnerNotFound, id)
	}
	if err != nil {
		return domain.Owner{}, fmt.Errorf("lock owner: %w", err)
	}
	return owner, nil
}

func (OwnerRepository) SetCachedBalance(ctx context.Context, tx *sql.Tx, id, balance int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE ledger_owners SET cached_balance = $2, updated_at = now() WHERE id = $1`, id, balance)
	if err != nil {
		return fmt.Errorf("update owner balance: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("owner balance affected rows: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("%w: %d", domain.ErrOwnerNotFound, id)
	}
	return nil
}

// singleRowQuerier is satisfied by both *sql.DB and *sql.Tx.
type singleRowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func normalizeMetadata(metadata domain.Metadata) domain.Metadata {
	if len(metadata) == 0 {
		return domain.EmptyMetadata()
	}
	return metadata
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
