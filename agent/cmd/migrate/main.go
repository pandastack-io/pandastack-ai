// SPDX-License-Identifier: Apache-2.0
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/pandastack/agent/internal/store"
)

func main() {
	dsnFlag := flag.String("dsn", "", "database DSN (defaults to PANDASTACK_DB_DSN, DATABASE_DIRECT_URL, or DATABASE_URL)")
	driverFlag := flag.String("driver", "", "database driver: sqlite or postgres (defaults to PANDASTACK_DB_DRIVER)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/migrate [--driver sqlite|postgres] [--dsn DSN] up|down|status")
		os.Exit(2)
	}
	driverName := *driverFlag
	if driverName == "" {
		driverName = os.Getenv("PANDASTACK_DB_DRIVER")
	}
	if driverName == "" {
		driverName = "sqlite"
	}
	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("PANDASTACK_DB_DSN")
	}
	if dsn == "" {
		dsn = os.Getenv("DATABASE_DIRECT_URL")
	}
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "missing DSN: set PANDASTACK_DB_DSN or pass --dsn")
		os.Exit(2)
	}
	db, err := store.OpenDBForDriver(driverName, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	if err := store.RunMigrationCommand(driverName, db, flag.Arg(0)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
