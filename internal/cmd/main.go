package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/syndbg/fabric-x-migrate-poc/internal/migrate"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "import" {
		return errors.New("usage: fabric-x-migrate import --snapshot CHANNEL=DIR --source-config CHANNEL=BLOCK --mapping FILE --target-config FILE --database-url URL")
	}
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var snapshots, sourceConfigs channelPaths
	flags.Var(&snapshots, "snapshot", "repeatable source CHANNEL=SNAPSHOT_DIRECTORY")
	flags.Var(&sourceConfigs, "source-config", "repeatable source CHANNEL=CONFIG_BLOCK")
	mapping := flags.String("mapping", "", "JSON target network and namespace mappings")
	target := flags.String("target-config", "", "Fabric-X initial CONFIG envelope or config block")
	databaseURL := flags.String("database-url", "", "committer PostgreSQL/YugabyteDB URL; pgx supports PGPASSFILE")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || len(snapshots) == 0 || len(sourceConfigs) == 0 || *mapping == "" || *target == "" || *databaseURL == "" {
		return errors.New("--snapshot, --source-config, --mapping, --target-config, and --database-url are required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		return fmt.Errorf("configure target database connection: %w", err)
	}
	defer pool.Close()
	result, err := migrate.Import(ctx, pool, migrate.Options{
		Snapshots: snapshots, SourceConfigs: sourceConfigs, MappingPath: *mapping, TargetConfigPath: *target,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

type channelPaths map[string]string

func (p *channelPaths) String() string {
	values := make([]string, 0, len(*p))
	for channel, path := range *p {
		values = append(values, channel+"="+path)
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func (p *channelPaths) Set(value string) error {
	channel, path, ok := strings.Cut(value, "=")
	if !ok || channel == "" || path == "" {
		return errors.New("expected CHANNEL=PATH")
	}
	if *p == nil {
		*p = channelPaths{}
	}
	if _, exists := (*p)[channel]; exists {
		return fmt.Errorf("channel %q is supplied more than once", channel)
	}
	(*p)[channel] = path
	return nil
}
