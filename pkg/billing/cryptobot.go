package billing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	apiToken string
	baseURL  string
	client   *http.Client
}

type CreateInvoiceRequest struct {
	Amount        string `json:"amount"`
	Asset         string `json:"asset"` // USDT, TON
	Description   string `json:"description"`
	Payload       string `json:"payload"` // userID
	PaidBtnName   string `json:"paid_btn_name,omitempty"`
	PaidBtnURL    string `json:"paid_btn_url,omitempty"`
	ExpiresIn     int    `json:"expires_in"` // seconds
}

type CreateInvoiceResponse struct {
	Ok     bool `json:"ok"`
	Result struct {
		InvoiceID int64  `json:"invoice_id"`
		PayURL    string `json:"bot_invoice_url"`
		Status    string `json:"status"`
		Amount    string `json:"amount"`
		Asset     string `json:"asset"`
	} `json:"result"`
	Error struct {
		Code int    `json:"code"`
		Name string `json:"name"`
	} `json:"error"`
}

func NewCryptoBot(apiToken string) *Client {
	return &Client{
		apiToken: apiToken,
		baseURL:  "https://pay.crypt.bot/api",
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) CreateInvoice(amount, asset, desc, payload string) (*CreateInvoiceResponse, error) {
	if c.apiToken == "" {
		return &CreateInvoiceResponse{
			Ok: true,
			Result: struct {
				InvoiceID int64  `json:"invoice_id"`
				PayURL    string `json:"bot_invoice_url"`
				Status    string `json:"status"`
				Amount    string `json:"amount"`
				Asset     string `json:"asset"`
			}{
				InvoiceID: time.Now().Unix(),
				PayURL:    "https://t.me/CryptoBot?start=mock_invoice",
				Status:    "active",
				Amount:    amount,
				Asset:     asset,
			},
		}, nil
	}

	reqBody, _ := json.Marshal(CreateInvoiceRequest{
		Amount:      amount,
		Asset:       asset,
		Description: desc,
		Payload:     payload,
		PaidBtnName: "openChannel",
		PaidBtnURL:  "https://pulse.nqai.es-cloud.ru",
		ExpiresIn:   3600,
	})

	req, err := http.NewRequest("POST", c.baseURL+"/createInvoice", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Crypto-Pay-API-Token", c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp CreateInvoiceResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, err
	}
	if !apiResp.Ok {
		return nil, fmt.Errorf("cryptobot error %d: %s", apiResp.Error.Code, apiResp.Error.Name)
	}
	return &apiResp, nil
}
