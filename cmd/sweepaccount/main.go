// Copyright (c) 2015-2023 The ltcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/ltcsuite/ltcd/btcjson"
	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/ltcutil"
	"github.com/ltcsuite/ltcd/rpcclient"
	"github.com/ltcsuite/ltcd/txscript"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/ltcwallet/internal/cfgutil"
	"github.com/ltcsuite/ltcwallet/netparams"
	"github.com/ltcsuite/ltcwallet/wallet/txauthor"
	"github.com/ltcsuite/ltcwallet/wallet/txrules"
	"github.com/ltcsuite/ltcwallet/wallet/txsizes"
	"golang.org/x/term"
)

const (
	defaultRPCTimeout    = 30 * time.Second
	defaultWalletTimeout = 60 * time.Second
	maxPassAttempts      = 3
	version              = "1.2.0"
)

var (
	walletDataDirectory = ltcutil.AppDataDir("ltcwallet", false)
)

type config struct {
	TestNet3              bool                `long:"testnet" description:"Use the test litecoin network (version 4)"`
	SimNet                bool                `long:"simnet" description:"Use the simulation bitcoin network"`
	RPCConnect            string              `short:"c" long:"connect" description:"Hostname[:port] of wallet RPC server"`
	RPCUsername           string              `short:"u" long:"rpcuser" description:"Wallet RPC username"`
	RPCCertificateFile    string              `long:"cafile" description:"Wallet RPC TLS certificate"`
	FeeRate               *cfgutil.AmountFlag `long:"feerate" description:"Transaction fee per kilobyte"`
	SourceAccount         string              `long:"sourceacct" description:"Account to sweep outputs from"`
	DestinationAccount    string              `long:"destacct" description:"Account to send sweeped outputs to"`
	RequiredConfirmations int64               `long:"minconf" description:"Required confirmations to include an output"`
	Version               bool                `long:"version" description:"Display version information and exit"`
}

type sweepResult struct {
	txHash       *chainhash.Hash
	amount       ltcutil.Amount
	err          error
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	if cfg.Version {
		fmt.Printf("ltcwallet-sweep v%s\n", version)
		return nil
	}

	rpcPassword, err := promptSecret("Wallet RPC password", maxPassAttempts)
	if err != nil {
		return fmt.Errorf("failed to get RPC password: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultRPCTimeout)
	defer cancel()

	rpcClient, err := createRPCClient(ctx, cfg, rpcPassword)
	if err != nil {
		return fmt.Errorf("RPC connection failed: %w", err)
	}
	defer rpcClient.Shutdown()

	unspentOutputs, err := rpcClient.ListUnspent()
	if err != nil {
		return fmt.Errorf("failed to fetch unspent outputs: %w", err)
	}

	sourceOutputs := filterOutputs(unspentOutputs, cfg.SourceAccount, cfg.RequiredConfirmations)
	if len(sourceOutputs) == 0 {
		fmt.Println("No outputs to sweep")
		return nil
	}

	privatePassphrase, err := promptSecret("Wallet private passphrase", maxPassAttempts)
	if err != nil {
		return fmt.Errorf("failed to get private passphrase: %w", err)
	}

	results := make(chan sweepResult, len(sourceOutputs))
	var totalSwept atomic.Int64
	var errorCount atomic.Int32

	for addr, outputs := range sourceOutputs {
		go func(addr string, outputs []btcjson.ListUnspentResult) {
			res := sweepOutputs(rpcClient, outputs, addr, cfg.DestinationAccount, 
				cfg.FeeRate.Amount, privatePassphrase)
			if res.err == nil {
				totalSwept.Add(int64(res.amount))
			} else {
				errorCount.Add(1)
			}
			results <- res
		}(addr, outputs)
	}

	// Collect and print results
	for range sourceOutputs {
		res := <-results
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "Failed to sweep %s: %v\n", res.txHash, res.err)
			continue
		}
		fmt.Printf("Swept %v to destination account with transaction %v\n",
			res.amount, res.txHash)
	}

	total := ltcutil.Amount(totalSwept.Load())
	fmt.Printf("Successfully swept %v across %d transactions\n", 
		total, len(sourceOutputs)-int(errorCount.Load()))

	if errorCount.Load() > 0 {
		return fmt.Errorf("failed to sweep %d outputs", errorCount.Load())
	}
	return nil
}

func loadConfig() (*config, error) {
	cfg := &config{
		RPCConnect:         "localhost",
		RPCCertificateFile: filepath.Join(walletDataDirectory, "rpc.cert"),
		FeeRate:            cfgutil.NewAmountFlag(txrules.DefaultRelayFeePerKb),
		SourceAccount:      "imported",
		DestinationAccount: "default",
		RequiredConfirmations: 1,
	}

	parser := flags.NewParser(cfg, flags.Default)
	if _, err := parser.Parse(); err != nil {
		return nil, err
	}

	if cfg.TestNet3 && cfg.SimNet {
		return nil, errors.New("multiple networks may not be used simultaneously")
	}

	if cfg.RPCConnect == "" {
		return nil, errors.New("RPC hostname[:port] is required")
	}

	if cfg.RPCUsername == "" {
		return nil, errors.New("RPC username is required")
	}

	if _, err := os.Stat(cfg.RPCCertificateFile); err != nil {
		return nil, fmt.Errorf("RPC certificate file not found: %w", err)
	}

	if cfg.FeeRate.Amount > 1e6 {
		return nil, fmt.Errorf("fee rate %v/kB is exceptionally high", cfg.FeeRate.Amount)
	}

	if cfg.FeeRate.Amount < 1e2 {
		return nil, fmt.Errorf("fee rate %v/kB is exceptionally low", cfg.FeeRate.Amount)
	}

	if cfg.SourceAccount == cfg.DestinationAccount {
		return nil, errors.New("source and destination accounts should not be equal")
	}

	if cfg.RequiredConfirmations < 0 {
		return nil, errors.New("required confirmations must be non-negative")
	}

	return cfg, nil
}

func createRPCClient(ctx context.Context, cfg *config, password string) (*rpcclient.Client, error) {
	cert, err := os.ReadFile(cfg.RPCCertificateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read RPC certificate: %w", err)
	}

	connCfg := &rpcclient.ConnConfig{
		Host:         cfg.RPCConnect,
		User:         cfg.RPCUsername,
		Pass:         password,
		Certificates: cert,
		HTTPPostMode: true,
	}

	return rpcclient.New(connCfg, nil)
}

func filterOutputs(outputs []btcjson.ListUnspentResult, account string, minConf int64) map[string][]btcjson.ListUnspentResult {
	result := make(map[string][]btcjson.ListUnspentResult)
	for _, output := range outputs {
		if !output.Spendable || 
		   output.Confirmations < minConf || 
		   output.Account != account {
			continue
		}
		result[output.Address] = append(result[output.Address], output)
	}
	return result
}

func sweepOutputs(
	client *rpcclient.Client,
	outputs []btcjson.ListUnspentResult,
	sourceAddr string,
	destAccount string,
	feeRate ltcutil.Amount,
	passphrase string,
) sweepResult {
	inputSource := createInputSource(outputs)
	destSource := createDestinationSource(client, destAccount)

	tx, err := txauthor.NewUnsignedTransaction(nil, feeRate, inputSource, destSource)
	if err != nil {
		return sweepResult{err: fmt.Errorf("create tx failed: %w", err)}
	}

	if err := client.WalletPassphrase(passphrase, defaultWalletTimeout); err != nil {
		return sweepResult{err: fmt.Errorf("unlock failed: %w", err)}
	}
	defer client.WalletLock()

	signedTx, complete, err := client.SignRawTransaction(tx.Tx)
	if err != nil {
		return sweepResult{err: fmt.Errorf("sign failed: %w", err)}
	}
	if !complete {
		return sweepResult{err: errors.New("not all inputs signed")}
	}

	txHash, err := client.SendRawTransaction(signedTx, false)
	if err != nil {
		return sweepResult{err: fmt.Errorf("send failed: %w", err)}
	}

	return sweepResult{
		txHash: txHash,
		amount: ltcutil.Amount(tx.Tx.TxOut[0].Value),
	}
}

func createInputSource(outputs []btcjson.ListUnspentResult) txauthor.InputSource {
	var (
		total ltcutil.Amount
		inputs []*wire.TxIn
		values []ltcutil.Amount
	)

	for _, output := range outputs {
		amount, err := ltcutil.NewAmount(output.Amount)
		if err != nil || amount == 0 || !saneOutputValue(amount) {
			continue
		}

		outpoint, err := parseOutPoint(&output)
		if err != nil {
			continue
		}

		total += amount
		inputs = append(inputs, wire.NewTxIn(&outpoint, nil, nil))
		values = append(values, amount)
	}

	return func(ltcutil.Amount) (ltcutil.Amount, []*wire.TxIn, []ltcutil.Amount, [][]byte, error) {
		if total == 0 {
			return 0, nil, nil, nil, errors.New("no input value")
		}
		return total, inputs, values, nil, nil
	}
}

func createDestinationSource(client *rpcclient.Client, account string) *txauthor.ChangeSource {
	return &txauthor.ChangeSource{
		ScriptSize: txsizes.P2PKHPkScriptSize,
		NewScript: func() ([]byte, error) {
			addr, err := client.GetNewAddress(account)
			if err != nil {
				return nil, err
			}
			return txscript.PayToAddrScript(addr)
		},
	}
}

func promptSecret(prompt string, maxAttempts int) (string, error) {
	for i := 0; i < maxAttempts; i++ {
		fmt.Printf("%s: ", prompt)
		pass, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		if len(pass) > 0 {
			return string(pass), nil
		}
	}
	return "", errors.New("maximum attempts reached")
}

func saneOutputValue(amount ltcutil.Amount) bool {
	return amount >= 0 && amount <= ltcutil.MaxSatoshi
}

func parseOutPoint(input *btcjson.ListUnspentResult) (wire.OutPoint, error) {
	txHash, err := chainhash.NewHashFromStr(input.TxID)
	if err != nil {
		return wire.OutPoint{}, err
	}
	return wire.OutPoint{Hash: *txHash, Index: input.Vout}, nil
}
