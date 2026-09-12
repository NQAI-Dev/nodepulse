package protocol

type InvoiceRequest struct {
	Plan string `json:"plan"` // pro
}

type InvoiceResponse struct {
	InvoiceID string `json:"invoice_id"`
	PayURL    string `json:"pay_url"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
}

type CryptoBotWebhook struct {
	UpdateID int64 `json:"update_id"`
	Payload  struct {
		InvoiceID int64  `json:"invoice_id"`
		Status    string `json:"status"` // paid
		Payload   string `json:"payload"` // user_id
		Amount    string `json:"amount"`
		Asset     string `json:"asset"`
	} `json:"payload"`
}
