package nodeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAuthenticatedPoolAPI(t *testing.T) {
	const token = "pool-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/miner/getblocktemplate":
			var request struct {
				LongPollID string  `json:"longpollid"`
				WorkNonce  *uint64 `json:"worknonce"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.LongPollID != "" || request.WorkNonce == nil || *request.WorkNonce != 51 {
				t.Errorf("wrong template request: %+v, %v", request, err)
			}
			_ = json.NewEncoder(w).Encode(Template{LongPollID: "next", WorkNonce: 51})
		case "/api/miner/submitblock":
			_ = json.NewEncoder(w).Encode(map[string]string{"block": "block-id"})
		case "/api/miner/blockstatus":
			_ = json.NewEncoder(w).Encode(BlockStatus{Block: "block-id", Known: true, Canonical: true})
		case "/api/status":
			_ = json.NewEncoder(w).Encode(Status{Network: "qday-mainnet", Height: 5, Synced: true})
		case "/api/unlock":
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case "/api/pool/payout":
			_ = json.NewEncoder(w).Encode(PayoutResponse{RequestID: "request", Transaction: "tx", Status: "queued"})
		case "/api/pool/defend":
			_ = json.NewEncoder(w).Encode(DefendResponse{Status: "idle"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	nonce := uint64(51)
	if template, err := client.GetBlockTemplate(context.Background(), "", &nonce); err != nil || template.WorkNonce != nonce {
		t.Fatalf("template: %+v, %v", template, err)
	}
	if id, err := client.SubmitBlock(context.Background(), "block"); err != nil || id != "block-id" {
		t.Fatalf("submit: %q, %v", id, err)
	}
	if status, err := client.BlockStatus(context.Background(), "block-id"); err != nil || !status.Canonical {
		t.Fatalf("block status: %+v, %v", status, err)
	}
	if status, err := client.Status(context.Background()); err != nil || !status.Synced {
		t.Fatalf("node status: %+v, %v", status, err)
	}
	if err := client.Unlock(context.Background(), "password"); err != nil {
		t.Fatal(err)
	}
	if payout, err := client.Payout(context.Background(), PayoutRequest{RequestID: "request"}); err != nil || payout.Transaction != "tx" {
		t.Fatalf("payout: %+v, %v", payout, err)
	}
	if defend, err := client.Defend(context.Background()); err != nil || defend.Status != "idle" {
		t.Fatalf("defend: %+v, %v", defend, err)
	}
}

func TestNodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "wallet is locked"})
	}))
	defer server.Close()
	client, _ := New(server.URL, "token")
	if _, err := client.Status(context.Background()); err == nil || err.Error() != "wallet is locked" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTemplateRetriesTransientTransportFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack first request: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		_ = json.NewEncoder(w).Encode(Template{LongPollID: "recovered", Height: 42})
	}))
	defer server.Close()

	client, err := New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	template, err := client.GetBlockTemplate(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	} else if template.LongPollID != "recovered" || template.Height != 42 {
		t.Fatalf("wrong recovered template: %+v", template)
	} else if requests.Load() != 2 {
		t.Fatalf("template requests = %d, want 2", requests.Load())
	}
}

func TestTemplateDoesNotRetryNodeError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "template rejected"})
	}))
	defer server.Close()

	client, err := New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetBlockTemplate(context.Background(), "", nil); err == nil || err.Error() != "template rejected" {
		t.Fatalf("unexpected template error: %v", err)
	} else if requests.Load() != 1 {
		t.Fatalf("template requests = %d, want 1", requests.Load())
	}
}
