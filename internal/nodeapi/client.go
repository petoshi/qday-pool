// Package nodeapi talks to QDAY's authenticated local pool API.
package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseSize = 64 << 20

type TemplateTransaction struct {
	Data   string `json:"data"`
	TxID   string `json:"txid"`
	TxType string `json:"txtype"`
}

type StratumTemplate struct {
	Block        string   `json:"block"`
	MerkleBranch []string `json:"merklebranch"`
}

type Template struct {
	Header            string                `json:"header"`
	Commitment        string                `json:"commitment"`
	Transactions      []TemplateTransaction `json:"transactions"`
	PreviousBlockHash string                `json:"previousblockhash"`
	LongPollID        string                `json:"longpollid"`
	Target            string                `json:"target"`
	Height            uint64                `json:"height"`
	Timestamp         int64                 `json:"curtime"`
	Bits              string                `json:"bits"`
	WorkNonce         uint64                `json:"worknonce"`
	BlockRewardAtomic string                `json:"blockRewardAtomic"`
	FeesAtomic        string                `json:"feesAtomic"`
	PayoutAtomic      string                `json:"payoutAtomic"`
	Stratum           StratumTemplate       `json:"stratum"`
}

type BlockStatus struct {
	Block          string `json:"block"`
	Known          bool   `json:"known"`
	Canonical      bool   `json:"canonical"`
	Height         uint64 `json:"height"`
	TipHeight      uint64 `json:"tipHeight"`
	Confirmations  uint64 `json:"confirmations"`
	MaturityHeight uint64 `json:"maturityHeight"`
}

type Status struct {
	Network      string `json:"network"`
	Height       uint64 `json:"height"`
	Synced       bool   `json:"synced"`
	Unlocked     bool   `json:"unlocked"`
	HasWallet    bool   `json:"hasWallet"`
	Address      string `json:"address"`
	Unit         string `json:"unit"`
	Balance      string `json:"balance"`
	Immature     string `json:"immature"`
	Pending      string `json:"pending"`
	BalanceReady bool   `json:"balanceReady"`
	Peers        int    `json:"peers"`
	Mempool      int    `json:"mempoolTransactions"`
	Qday         bool   `json:"qday"`
}

type PayoutOutput struct {
	Address      string `json:"address"`
	AmountAtomic string `json:"amountAtomic"`
}

type PayoutRequest struct {
	RequestID          string         `json:"requestID"`
	FromAddress        string         `json:"fromAddress"`
	ExpectedUnitAtomic string         `json:"expectedUnitAtomic"`
	FeeAtomic          string         `json:"feeAtomic"`
	Outputs            []PayoutOutput `json:"outputs"`
}

type PayoutResponse struct {
	RequestID   string `json:"requestID"`
	Transaction string `json:"transaction"`
	Outputs     int    `json:"outputs"`
	Status      string `json:"status"`
}

type DefendResponse struct {
	Status      string `json:"status"`
	Transaction string `json:"transaction,omitempty"`
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("node URL must look like http://127.0.0.1:19770")
	}
	if token = strings.TrimSpace(token); token == "" {
		return nil, errors.New("QDAY API token is empty")
	}
	return &Client{baseURL: baseURL, token: token, http: &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
	}}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, result any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return err
	} else if len(payload) > maxResponseSize {
		return errors.New("QDAY node response exceeds 64 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return fmt.Errorf("QDAY node returned HTTP %d", resp.StatusCode)
	}
	if result != nil && json.Unmarshal(payload, result) != nil {
		return errors.New("QDAY node returned invalid JSON")
	}
	return nil
}

func (c *Client) GetBlockTemplate(ctx context.Context, longPollID string, workNonce *uint64) (Template, error) {
	var result Template
	err := c.request(ctx, http.MethodPost, "/api/miner/getblocktemplate", struct {
		LongPollID string  `json:"longpollid,omitempty"`
		WorkNonce  *uint64 `json:"worknonce,omitempty"`
	}{longPollID, workNonce}, &result)
	return result, err
}

func (c *Client) SubmitBlock(ctx context.Context, block string) (string, error) {
	var result struct {
		Block string `json:"block"`
	}
	err := c.request(ctx, http.MethodPost, "/api/miner/submitblock", struct {
		Params []string `json:"params"`
	}{[]string{block}}, &result)
	return result.Block, err
}

func (c *Client) BlockStatus(ctx context.Context, block string) (BlockStatus, error) {
	var result BlockStatus
	err := c.request(ctx, http.MethodPost, "/api/miner/blockstatus", struct {
		Block string `json:"block"`
	}{block}, &result)
	return result, err
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var result Status
	err := c.request(ctx, http.MethodGet, "/api/status", nil, &result)
	return result, err
}

func (c *Client) Unlock(ctx context.Context, password string) error {
	return c.request(ctx, http.MethodPost, "/api/unlock", struct {
		Password string `json:"password"`
	}{password}, new(any))
}

func (c *Client) Payout(ctx context.Context, payout PayoutRequest) (PayoutResponse, error) {
	var result PayoutResponse
	err := c.request(ctx, http.MethodPost, "/api/pool/payout", payout, &result)
	return result, err
}

func (c *Client) Defend(ctx context.Context) (DefendResponse, error) {
	var result DefendResponse
	err := c.request(ctx, http.MethodPost, "/api/pool/defend", struct{}{}, &result)
	return result, err
}
