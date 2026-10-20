package storage

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func TestAdoptCanonicalZeroRowsRereadsCanonical(t *testing.T) {
	t.Parallel()
	stored := "https://github.com/Acme/Widgets.git"
	canonical := config.RemoteIdentity(stored)
	require.NotEqual(t, stored, canonical)
	db := &scriptRepoDB{
		scans: []scanRow{
			{err: pgx.ErrNoRows},
			{id: 5},
			{id: 9},
		},
		tag: pgconn.NewCommandTag("UPDATE 0"),
	}
	id, ok, err := adoptCanonicalRepoIdentity(context.Background(), db, stored)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(9), id)
	assert.Equal(t, 1, db.execs)
	assert.Equal(t, []string{stored, canonical, canonical}, db.lookups)
}

type scanRow struct {
	id  int64
	err error
}

func (r scanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64) = r.id
	return nil
}

type scriptRepoDB struct {
	scans   []scanRow
	n       int
	tag     pgconn.CommandTag
	execs   int
	lookups []string
}

func (s *scriptRepoDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if len(args) > 0 {
		if identity, ok := args[0].(string); ok {
			s.lookups = append(s.lookups, identity)
		}
	}
	i := s.n
	s.n++
	if i >= len(s.scans) {
		return scanRow{err: pgx.ErrNoRows}
	}
	return s.scans[i]
}

func (s *scriptRepoDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	s.execs++
	return s.tag, nil
}

// A credential in the query or fragment must not be written over the
// canonical Postgres row. Without adoption the identity is handled as on main.
func TestAdoptCanonicalSkipsCredentialQuery(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{
		"https://git.example/org/repo.git?access_token=synthetic-secret",
		"https://git.example/org/repo.git#private_token=synthetic-secret",
	} {
		require.NotEqual(t, identity, config.RemoteIdentity(identity))
		db := &scriptRepoDB{
			scans: []scanRow{{err: pgx.ErrNoRows}, {id: 5}},
			tag:   pgconn.NewCommandTag("UPDATE 1"),
		}
		_, ok, err := adoptCanonicalRepoIdentity(context.Background(), db, identity)
		require.NoError(t, err)
		assert.False(t, ok, identity)
		assert.Equal(t, 0, db.execs, identity)
	}
}
