package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	rh "github.com/hashicorp/go-retryablehttp"
)

type Config struct {
	MercuryAPIKey     string `json:"mercuryAPIKey"`
	InvoiceNinjaToken string `json:"invoiceNinjaToken"`
	InvoiceNinjaURL   string `json:"invoiceNinjaURL"`
	BankProvider      string `json:"invoiceNinjaBankProvider"`
	SyncIntervalHours int    `json:"syncIntervalHours"`
	SyncStartDaysAgo  int    `json:"syncStartDaysAgo"`
	LogLevel          string `json:"logLevel"`

	stateFilePath     string
	bankIntegrationID string
	mercuryAccounts   []*MercuryAccount
}

type SyncState struct {
	ProcessedTxIDs map[string]time.Time `json:"processed_tx_ids"`
}

type MercuryAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type MercuryTransaction struct {
	ID              string    `json:"id"`
	Amount          float64   `json:"amount"`
	BankDescription string    `json:"bankDescription"`
	PostedAt        time.Time `json:"postedAt"`
}

type InvoiceNinjaBankTX struct {
	Amount            float64 `json:"amount"`
	Date              string  `json:"date"`
	Description       string  `json:"description"`
	BankIntegrationID string  `json:"bank_integration_id"`
	BaseType          string  `json:"base_type"`
}

type BankIntegration struct {
	ID           string `json:"id"`
	ProviderName string `json:"provider_name"`
}

func loadConfig(configPath, dataDir, invoiceNinjaURL string) (*Config, error) {
	config := &Config{
		SyncIntervalHours: 1,
		SyncStartDaysAgo:  7, // Typical time for bank transactions is 3–5 days
		LogLevel:          "info",
		BankProvider:      "Mercury",
		stateFilePath:     filepath.Join(dataDir, "sync_state.json"),
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %v", err)
	}

	if err := json.Unmarshal(configData, config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %v", err)
	}

	if config.MercuryAPIKey == "" {
		return nil, fmt.Errorf("missing Mercury API key")
	}
	if config.InvoiceNinjaToken == "" {
		return nil, fmt.Errorf("missing InvoiceNinja token")
	}

	if config.InvoiceNinjaURL == "" {
		config.InvoiceNinjaURL = invoiceNinjaURL
	}
	if _, err := url.ParseRequestURI(config.InvoiceNinjaURL); err != nil {
		return nil, fmt.Errorf("invalid InvoiceNinja URL: %v", err)
	}

	return config, nil
}

func loadState(stateFilePath string) (*SyncState, error) {
	state := &SyncState{
		ProcessedTxIDs: make(map[string]time.Time),
	}

	if _, err := os.Stat(stateFilePath); os.IsNotExist(err) {
		slog.Debug("No state file found, using default state")
		return state, nil
	}

	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		return nil, fmt.Errorf("error reading state file: %v", err)
	}

	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("error parsing state file: %v", err)
	}

	slog.Debug("Loaded state", "processed_tx_count", len(state.ProcessedTxIDs))
	return state, nil
}

func saveState(stateFilePath string, state *SyncState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("error serializing state: %v", err)
	}

	dir := filepath.Dir(stateFilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("error creating state directory: %v", err)
	}

	if err := os.WriteFile(stateFilePath, data, 0644); err != nil {
		return fmt.Errorf("error writing state file: %v", err)
	}

	return nil
}

var retryClient = rh.NewClient()

func submitRequest(req *rh.Request, res any) error {
	resp, err := retryClient.Do(req)
	if err != nil {
		return fmt.Errorf("error submitting request: %s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("error submitting request: %s %s: %d %s",
			req.Method, req.URL, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response body: %v", err)
	}
	slog.Debug("API response", "method", req.Method, "url", req.URL,
		"status", resp.StatusCode, "body", string(body))

	if err := json.Unmarshal(body, res); err != nil {
		return fmt.Errorf("error parsing JSON response: %s %s: %s %v",
			req.Method, req.URL, string(body), err)
	}
	return nil
}

func getRequest(method string, url string, headers map[string]string, body any) (*rh.Request, error) {
	if body != nil {
		body, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("error marshaling body: %s %s: %s %v", method, url, string(body), err)
		}
	}
	req, err := rh.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %s %s: %v", method, url, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func getMercuryRequest(config *Config, method string, url string, body any) (*rh.Request, error) {
	headers := map[string]string{
		"Authorization": "Bearer " + config.MercuryAPIKey,
	}
	return getRequest(method, "https://api.mercury.com/api/v1"+url, headers, body)
}

func fetchMercuryAccounts(config *Config) error {
	slog.Debug("Fetching Mercury accounts")

	req, err := getMercuryRequest(config, "GET", "/accounts", nil)
	if err != nil {
		return err
	}
	var res struct {
		Accounts []*MercuryAccount `json:"accounts"`
	}
	if err = submitRequest(req, &res); err != nil {
		return err
	}
	config.mercuryAccounts = res.Accounts
	return nil
}

func fetchMercuryTransactions(config *Config, acct *MercuryAccount) ([]*MercuryTransaction, error) {
	start := time.Now().AddDate(0, 0, -config.SyncStartDaysAgo).Format(time.RFC3339)
	slog.Debug("Fetching Mercury transactions", "account", acct.Name, "since", start)

	url := fmt.Sprintf("/account/%s/transactions?status=sent&start=%s", acct.ID, start)
	req, err := getMercuryRequest(config, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Transactions []*MercuryTransaction `json:"transactions"`
	}
	if err = submitRequest(req, &res); err != nil {
		return nil, err
	}
	return res.Transactions, nil
}

func getInvoiceNinjaRequest(config *Config, method string, url string, body any) (*rh.Request, error) {
	headers := map[string]string{
		"X-API-Token":      config.InvoiceNinjaToken,
		"X-Requested-With": "XMLHttpRequest",
	}
	return getRequest(method, config.InvoiceNinjaURL+"/api/v1"+url, headers, body)
}

func fetchBankIntegrationID(config *Config) error {
	slog.Debug("Fetching InvoiceNinja bank integration")

	req, err := getInvoiceNinjaRequest(config, "GET", "/bank_integrations", nil)
	if err != nil {
		return err
	}
	var res struct {
		Integrations []*BankIntegration `json:"data"`
	}
	if err = submitRequest(req, &res); err != nil {
		return err
	}

	for _, ig := range res.Integrations {
		if ig.ProviderName == config.BankProvider {
			slog.Debug("Found bank integration", "provider", config.BankProvider, "id", ig.ID)
			config.bankIntegrationID = ig.ID
			return nil
		}
	}
	return fmt.Errorf("no bank integration found for provider: %s", config.BankProvider)
}

func createInvoiceNinjaTransaction(config *Config, tx *MercuryTransaction) error {
	slog.Debug("Creating bank transaction in InvoiceNinja",
		"amount", tx.Amount, "description", tx.BankDescription)

	baseType := "DEBIT"
	if tx.Amount > 0 {
		baseType = "CREDIT"
	}

	req, err := getInvoiceNinjaRequest(config, "POST", "/bank_transactions", InvoiceNinjaBankTX{
		Amount:            math.Abs(tx.Amount),
		Date:              tx.PostedAt.Format("2006-01-02"),
		Description:       tx.BankDescription,
		BankIntegrationID: config.bankIntegrationID,
		BaseType:          baseType,
	})
	if err != nil {
		return err
	}

	return submitRequest(req, &struct {
		Data InvoiceNinjaBankTX `json:"data"`
	}{})
}

func syncTransactions(config *Config, state *SyncState) error {
	cutoffTime := time.Now().AddDate(0, 0, -7)

	for id, timestamp := range state.ProcessedTxIDs {
		if timestamp.Before(cutoffTime) {
			delete(state.ProcessedTxIDs, id)
		}
	}

	totalProcessed := 0
	for _, acct := range config.mercuryAccounts {
		slog.Debug("Processing account", "name", acct.Name)

		txs, err := fetchMercuryTransactions(config, acct)
		if err != nil {
			slog.Error("Error fetching transactions", "account", acct.Name, "error", err)
			continue
		}
		if len(txs) == 0 {
			continue
		}
		slog.Debug("Processing transactions", "account", acct.Name, "count", len(txs))

		processed := 0
		for _, tx := range txs {
			if _, ok := state.ProcessedTxIDs[tx.ID]; ok {
				slog.Debug("Skipping already processed transaction", "id", tx.ID)
				continue
			}

			if err := createInvoiceNinjaTransaction(config, tx); err != nil {
				return err
			} else {
				state.ProcessedTxIDs[tx.ID] = time.Now()
				totalProcessed++
			}
			processed++
		}
		if processed > 0 {
			slog.Info("Account sync completed", "account", acct.Name, "transactions", processed)
		}
	}

	slog.Debug("Sync completed", "transactions", totalProcessed)
	return nil
}

func setupLog(logLevel string) {
	level := slog.LevelInfo
	switch strings.ToLower(logLevel) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)
}

func setupHttpClient() {
	retryClient.RetryMax = 5
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		retryClient.Logger = nil
	}
}

func main() {
	configPath := flag.String("c", "/config.json", "Path to config file")
	dataDir := flag.String("d", "/data", "Directory for storing state")
	invoiceNinjaURL := flag.String("i", "", "InvoiceNinja URL")
	flag.Parse()

	config, err := loadConfig(*configPath, *dataDir, *invoiceNinjaURL)
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	setupLog(config.LogLevel)
	setupHttpClient()

	state, err := loadState(config.stateFilePath)
	if err != nil {
		log.Fatalf("Error loading state: %v", err)
	}

	if err = fetchBankIntegrationID(config); err != nil {
		log.Fatalf("Error fetching bank integration ID: %v", err)
	}

	if err = fetchMercuryAccounts(config); err != nil {
		log.Fatalf("Error fetching Mercury accounts: %v", err)
	}

	for {
		if err := syncTransactions(config, state); err != nil {
			slog.Error("Error in sync", "error", err)
		} else if err := saveState(config.stateFilePath, state); err != nil {
			slog.Error("Error saving state", "error", err)
		}

		nextSync := time.Now().Add(time.Duration(config.SyncIntervalHours) * time.Hour)
		slog.Debug("Waiting for next sync", "next_sync", nextSync.Format(time.RFC3339))
		time.Sleep(time.Until(nextSync))
	}
}
