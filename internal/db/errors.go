package db

import (
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

func itoa(n int) string { return strconv.Itoa(n) }

// IsUniqueViolation reports whether err is a PostgreSQL unique-constraint error.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsForeignKeyViolation reports whether err is a PostgreSQL foreign-key error.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
