// Copyright (c) 2015-2023 The ltcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/ltcsuite/ltcd/ltcutil"
	"github.com/ltcsuite/ltcwallet/wallet"
	"github.com/ltcsuite/ltcwallet/walletdb"
	_ "github.com/ltcsuite/ltcwallet/walletdb/bdb"
	"golang.org/x/term"
)

const (
	defaultNet          = "mainnet"
	defaultDBTimeout    = 30 * time.Second
	maxInteractiveTries = 3
)

var (
	// datadir defines the default data directory
	datadir = ltcutil.AppDataDir("ltcwallet", false)
	
	// Version indicates the current version of the tool
	Version = "0.1.0-dev"
)

// config defines the configuration options
type config struct {
	Force      bool          `short:"f" long:"force" description:"Force removal without prompt"`
	DbPath     string        `long:"db" description:"Path to wallet database"`
	DropLabels bool          `long:"droplabels" description:"Drop transaction labels"`
	Timeout    time.Duration `long:"timeout" description:"Timeout value when opening the wallet database"`
	Version    bool          `long:"version" description:"Display version information and exit"`
}

// loadConfig initializes and parses the config
func loadConfig() (*config, error) {
	cfg := &config{
		DbPath:  filepath.Join(datadir, defaultNet, wallet.WalletDBName),
		Timeout: defaultDBTimeout,
	}

	parser := flags.NewParser(cfg, flags.Default)
	_, err := parser.Parse()
	if err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		return nil, err
	}

	if cfg.Version {
		fmt.Printf("ltcwallet-dbclean v%s\n", Version)
		os.Exit(0)
	}

	return cfg, nil
}

// confirmAction prompts for confirmation
func confirmAction(prompt string, maxTries int) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, errors.New("standard input is not a terminal")
	}

	reader := bufio.NewReader(os.Stdin)
	for i := 0; i < maxTries; i++ {
		fmt.Printf("%s [y/N] ", prompt)

		answer, err := reader.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("failed to read input: %v", err)
		}
		answer = strings.TrimSpace(strings.ToLower(answer))

		switch answer {
		case "y", "yes":
			return true, nil
		case "n", "no", "":
			return false, nil
		default:
			fmt.Println("Please answer 'yes' or 'no'")
		}
	}

	return false, errors.New("maximum tries exceeded")
}

// run executes the main program logic
func run(ctx context.Context) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing config: %v\n", err)
		return 1
	}

	fmt.Println("Database path:", cfg.DbPath)

	// Check if database exists
	if _, err := os.Stat(cfg.DbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Database file does not exist at %s\n", cfg.DbPath)
		return 1
	}

	// Get confirmation if not forced
	if !cfg.Force {
		confirmed, err := confirmAction("Drop all ltcwallet transaction history?", maxInteractiveTries)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting confirmation: %v\n", err)
			return 1
		}
		if !confirmed {
			fmt.Println("Operation cancelled by user")
			return 0
		}
	}

	// Open database with context
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	db, err := walletdb.OpenWithTimeout("bdb", cfg.DbPath, true, cfg.Timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing database: %v\n", err)
		}
	}()

	fmt.Println("Dropping ltcwallet transaction history...")

	// Perform the destructive operation
	if err := wallet.DropTransactionHistory(db, !cfg.DropLabels); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to drop transaction history: %v\n", err)
		return 1
	}

	fmt.Println("Successfully dropped transaction history")
	return 0
}

func main() {
	ctx := context.Background()
	os.Exit(run(ctx))
}
