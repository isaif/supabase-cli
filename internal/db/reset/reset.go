package reset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v4"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"github.com/supabase/cli/internal/db/diff"
	"github.com/supabase/cli/internal/debug"
	"github.com/supabase/cli/internal/utils"
	"github.com/supabase/cli/internal/utils/parser"
)

func Run(ctx context.Context, fsys afero.Fs) error {
	// Sanity checks.
	{
		if err := utils.LoadConfigFS(fsys); err != nil {
			return err
		}
		if err := utils.AssertSupabaseDbIsRunning(); err != nil {
			return err
		}
	}

	branch, err := utils.GetCurrentBranchFS(fsys)
	if err != nil {
		// Assume we are on main branch
		branch = "main"
	}

	var opts []func(*pgx.ConnConfig)
	if viper.GetBool("DEBUG") {
		opts = append(opts, debug.SetupPGX)
	}

	fmt.Fprintln(os.Stderr, "Resetting database...")
	if err := diff.ResetDatabase(ctx, utils.DbId, branch); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Initialising schema...")
	url := fmt.Sprintf("postgresql://postgres:postgres@localhost:%d/%s", utils.Config.Db.Port, branch)
	if err := diff.ApplyMigrations(ctx, url, fsys, opts...); err != nil {
		return err
	}

	if err := SeedDatabase(ctx, url, fsys, opts...); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	fmt.Fprintln(os.Stderr, "Activating branch...")
	if err := ActivateDatabase(ctx, branch, opts...); err != nil {
		return err
	}

	// Reload PostgREST schema cache.
	if err := utils.Docker.ContainerKill(ctx, utils.RestId, "SIGUSR1"); err != nil {
		fmt.Fprintf(os.Stderr, "Error reloading PostgREST schema cache: %v", err)
	}

	fmt.Fprintln(os.Stderr, "Finished "+utils.Aqua("supabase db reset")+" on branch "+utils.Aqua(branch)+".")
	return nil
}

func SeedDatabase(ctx context.Context, url string, fsys afero.Fs, options ...func(*pgx.ConnConfig)) error {
	sql, err := fsys.Open(utils.SeedDataPath)
	if err != nil {
		return err
	}
	defer sql.Close()
	fmt.Fprintln(os.Stderr, "Seeding data...")
	// Parse connection url
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}
	// Apply config overrides
	for _, op := range options {
		op(config)
	}
	// Connect to database
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// Batch seed commands, safe to use statement cache
	batch := pgx.Batch{}
	for _, line := range parser.Split(sql) {
		trim := strings.TrimSpace(strings.TrimRight(line, ";"))
		if len(trim) > 0 {
			batch.Queue(trim)
		}
	}
	if err := conn.SendBatch(ctx, &batch).Close(); err != nil {
		return err
	}
	return nil
}

func ActivateDatabase(ctx context.Context, branch string, options ...func(*pgx.ConnConfig)) error {
	// Parse connection url
	url := fmt.Sprintf("postgresql://postgres:postgres@localhost:%d/template1", utils.Config.Db.Port)
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}
	// Apply config overrides
	for _, op := range options {
		op(config)
	}
	// Connect to database
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// Must be executed separately because running in transaction is unsupported
	disconn := "ALTER DATABASE postgres ALLOW_CONNECTIONS false;"
	if _, err := conn.Exec(ctx, disconn); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code != pgerrcode.InvalidCatalogName {
			return err
		}
	}
	term := fmt.Sprintf(utils.TerminateDbSqlFmt, "postgres")
	if _, err := conn.Exec(ctx, term); err != nil {
		return err
	}
	drop := "DROP DATABASE IF EXISTS postgres WITH (FORCE);"
	if _, err := conn.Exec(ctx, drop); err != nil {
		return err
	}
	swap := "ALTER DATABASE " + branch + " RENAME TO postgres;"
	if _, err := conn.Exec(ctx, swap); err != nil {
		return err
	}
	return nil
}
