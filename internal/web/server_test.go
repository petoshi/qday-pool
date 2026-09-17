package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petoshi/qday-pool/internal/nodeapi"
	"github.com/petoshi/qday-pool/internal/pool"
	"github.com/petoshi/qday-pool/internal/store"
	"github.com/petoshi/qday-pool/internal/stratum"
)

type staticMining struct{ value stratum.Snapshot }

func (s staticMining) Snapshot() stratum.Snapshot { return s.value }

type staticController struct{ value pool.Snapshot }

func (s staticController) Snapshot() pool.Snapshot { return s.value }

func TestDashboardAndReadOnlyAPI(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mining := staticMining{stratum.Snapshot{Ready: true, Height: 20, NetworkHashrate: 1e12, NetworkDifficulty: 2, NetworkWork: "60000000000000", Transactions: 3, Connected: 9, Authorized: 2, UpdatedAt: time.Now()}}
	controller := staticController{pool.Snapshot{Node: nodeapi.Status{Network: "qday-mainnet", Height: 20, Synced: true, Unit: "1000000", Peers: 4, Mempool: 9}, MinimumPayout: "1000000", PayoutFee: "1000"}}
	server, err := New(database, mining, controller, Config{StratumAddress: "stratum+tcp://pool.pqday.com:3333", PoolFeeBPS: 100, PPLNSWindow: 2})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	request := httptest.NewRequest(http.MethodGet, "http://pool.pqday.com/api/status", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status HTTP %d: %s", response.Code, response.Body.String())
	}
	var status Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || !status.Ready || status.Pool.Connected != 2 || status.Policy.FeePercent != 1 || status.Network.Transactions != 3 || status.Network.Mempool != 9 {
		t.Fatalf("wrong status: %+v, %v", status, err)
	}

	request = httptest.NewRequest(http.MethodGet, "http://pool.pqday.com/blocks", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "QDAY Pool") || response.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("wrong dashboard response: %d %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://pool.pqday.com/api/status", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write method returned %d", response.Code)
	}
}
