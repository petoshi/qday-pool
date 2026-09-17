package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/petoshi/qday-pool/internal/amount"
	"github.com/petoshi/qday-pool/internal/nodeapi"
	"github.com/petoshi/qday-pool/internal/pool"
	"github.com/petoshi/qday-pool/internal/store"
	"github.com/petoshi/qday-pool/internal/stratum"
	poolweb "github.com/petoshi/qday-pool/internal/web"
)

var version = "dev"

func defaultTokenFile() string {
	config, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(config, "qday", "qday-mainnet-d71aebcb687c", "api.token")
}

func parsePercentBPS(value string) (uint64, error) {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("pool fee must be a percentage with at most two decimal places")
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if fraction == "" || len(fraction) > 2 {
			return 0, errors.New("pool fee must have at most two decimal places")
		}
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || whole > 100 {
		return 0, errors.New("pool fee must be between 0 and 100 percent")
	}
	partial, err := strconv.ParseUint(fraction, 10, 64)
	if err != nil {
		return 0, errors.New("pool fee must be numeric")
	}
	bps := whole*100 + partial
	if bps > 10_000 {
		return 0, errors.New("pool fee must be between 0 and 100 percent")
	}
	return bps, nil
}

func readSecret(path, name string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s file path is empty", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s file: %w", name, err)
	}
	value := strings.TrimRight(string(b), "\r\n")
	if value == "" {
		return "", fmt.Errorf("%s file is empty", name)
	}
	return value, nil
}

func run() error {
	var (
		dataDir           = flag.String("data", "./pool-data", "pool database directory")
		stratumListen     = flag.String("stratum", ":3333", "public SiaMining Stratum listen address")
		httpListen        = flag.String("http", "127.0.0.1:8080", "dashboard HTTP listen address")
		publicStratum     = flag.String("public-stratum", "stratum+tcp://pool.pqday.com:3333", "Stratum URL shown to miners")
		nodeURL           = flag.String("node", "http://127.0.0.1:19770", "local QDAY node API URL")
		tokenFile         = flag.String("token-file", defaultTokenFile(), "path to the QDAY api.token file")
		passwordFile      = flag.String("wallet-password-file", "", "optional file containing the QDAY wallet password")
		poolFee           = flag.String("pool-fee", "1", "pool fee percentage; funds payout transaction fees")
		pplnsWindow       = flag.Uint64("pplns", 2, "PPLNS window in multiples of current network work")
		minimumPayout     = flag.String("minimum-payout", "1", "minimum displayed QDAY balance paid automatically")
		payoutFee         = flag.String("payout-fee", "0.001", "displayed QDAY fee paid by each payout batch")
		shareTarget       = flag.Duration("share-target", 15*time.Second, "target time between shares per worker")
		initialDifficulty = flag.Float64("initial-difficulty", 4, "initial SiaMining share difficulty")
		minimumDifficulty = flag.Float64("minimum-difficulty", .01, "minimum SiaMining share difficulty")
		maximumDifficulty = flag.Float64("maximum-difficulty", 1e12, "maximum SiaMining share difficulty")
		jobInterval       = flag.Duration("job-interval", time.Second, "fresh-job interval")
		maxMiners         = flag.Int("max-miners", 2048, "maximum simultaneous miner connections")
		maxMinersPerIP    = flag.Int("max-miners-per-ip", 64, "maximum simultaneous miner connections per IP")
		showVersion       = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Printf("qday-pool %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	}
	bps, err := parsePercentBPS(*poolFee)
	if err != nil {
		return err
	}
	token, err := readSecret(*tokenFile, "QDAY API token")
	if err != nil {
		return err
	}
	node, err := nodeapi.New(*nodeURL, token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	status, err := node.Status(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to QDAY node: %w", err)
	} else if status.Network != "qday-mainnet" {
		return fmt.Errorf("QDAY node is on %q, expected qday-mainnet", status.Network)
	} else if !status.HasWallet {
		return errors.New("create the pool payout wallet in the local QDAY node first")
	}
	if *passwordFile != "" && !status.Unlocked {
		password, err := readSecret(*passwordFile, "wallet password")
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = node.Unlock(ctx, password)
		cancel()
		if err != nil {
			return fmt.Errorf("unlock QDAY payout wallet: %w", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		status, err = node.Status(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	if !status.Unlocked {
		return errors.New("unlock the QDAY payout wallet or pass -wallet-password-file")
	}
	minimumAtomic, err := amount.Parse(*minimumPayout, status.Unit)
	if err != nil || minimumAtomic.Sign() <= 0 {
		return errors.New("minimum payout must be a positive QDAY amount")
	}
	if _, err := amount.Parse(*payoutFee, status.Unit); err != nil {
		return fmt.Errorf("invalid payout fee: %w", err)
	}
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		return err
	}
	database, err := store.Open(filepath.Join(*dataDir, "pool.sqlite3"))
	if err != nil {
		return err
	}
	defer database.Close()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mining, err := stratum.NewServer(node, database, stratum.Config{
		ListenAddress: *stratumListen, JobInterval: *jobInterval, ShareTarget: *shareTarget,
		InitialDifficulty: *initialDifficulty, MinimumDifficulty: *minimumDifficulty, MaximumDifficulty: *maximumDifficulty,
		PPLNSWindow: *pplnsWindow, PoolFeeBPS: bps, MaturityBlocks: 60,
		MaxMiners: *maxMiners, MaxMinersPerIP: *maxMinersPerIP, Logger: logger,
	})
	if err != nil {
		return err
	}
	controller, err := pool.New(node, database, pool.Config{MinimumPayout: *minimumPayout, PayoutFee: *payoutFee, MaxOutputs: 100, Logger: logger})
	if err != nil {
		return err
	}
	dashboard, err := poolweb.New(database, mining, controller, poolweb.Config{StratumAddress: *publicStratum, PoolFeeBPS: bps, PPLNSWindow: *pplnsWindow})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: *httpListen, Handler: dashboard.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}

	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go controller.Run(root)
	failures := make(chan error, 2)
	go func() { failures <- mining.Run(root) }()
	go func() {
		logger.Info("QDAY pool dashboard ready", "listen", *httpListen, "publicStratum", *publicStratum, "poolFeePercent", float64(bps)/100)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		failures <- err
	}()

	select {
	case <-root.Done():
	case err := <-failures:
		if err != nil {
			stop()
			return err
		}
		stop()
	}
	shutdown, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	return server.Shutdown(shutdown)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "qday-pool:", err)
		os.Exit(1)
	}
}
