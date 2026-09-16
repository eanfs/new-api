package service

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting"
	"github.com/shopspring/decimal"
	"golang.org/x/text/currency"
)

const antomMaxResponseBytes = 1 << 20

type AntomAmount struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}

// AntomMinorAmount rounds once, in the payment currency's ISO minor unit.
func AntomMinorAmount(amount decimal.Decimal, code string) (AntomAmount, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	unit, err := currency.ParseISO(code)
	if err != nil || unit == currency.XXX || !amount.IsPositive() {
		return AntomAmount{}, errors.New("invalid Antom amount or currency")
	}
	scale, _ := currency.Standard.Rounding(unit)
	value := amount.Shift(int32(scale)).Round(0).StringFixed(0)
	if value == "0" || len(value) > 16 {
		return AntomAmount{}, errors.New("Antom amount is outside the supported range")
	}
	return AntomAmount{Currency: code, Value: value}, nil
}

func AntomAmountMajor(amount AntomAmount) (decimal.Decimal, error) {
	unit, err := currency.ParseISO(amount.Currency)
	if err != nil || unit == currency.XXX || amount.Currency != strings.ToUpper(amount.Currency) || len(amount.Value) == 0 || len(amount.Value) > 16 {
		return decimal.Zero, errors.New("invalid Antom amount")
	}
	for _, c := range amount.Value {
		if c < '0' || c > '9' {
			return decimal.Zero, errors.New("Antom amount must use integer minor units")
		}
	}
	value, err := decimal.NewFromString(amount.Value)
	if err != nil || !value.IsPositive() {
		return decimal.Zero, errors.New("Antom amount must be positive")
	}
	scale, _ := currency.Standard.Rounding(unit)
	return value.Shift(-int32(scale)), nil
}

type AntomClient struct {
	ClientID           string
	Sandbox            bool
	gatewayURL         string
	settlementCurrency string
	merchantPrivateKey *rsa.PrivateKey
	antomPublicKey     *rsa.PublicKey
	httpClient         *http.Client
}

func NewAntomClient() (*AntomClient, error) {
	gateway := strings.TrimRight(strings.TrimSpace(setting.AntomGatewayUrl), "/")
	if gateway == "" {
		gateway = "https://open-sea-global.alipay.com"
	}
	switch gateway {
	case "https://open-sea-global.alipay.com", "https://open-na-global.alipay.com", "https://open-de-global.alipay.com":
	default:
		return nil, errors.New("unsupported Antom regional gateway")
	}
	clientID := strings.TrimSpace(setting.AntomClientId)
	if clientID == "" || len(clientID) > 64 || strings.ContainsAny(clientID, "\r\n\t ") {
		return nil, errors.New("invalid Antom client ID")
	}
	privateKey, err := parseAntomPrivateKey(setting.AntomMerchantPrivateKey)
	if err != nil {
		return nil, err
	}
	publicKey, err := parseAntomPublicKey(setting.AntomPublicKey)
	if err != nil {
		return nil, err
	}
	settlementCurrency := strings.ToUpper(strings.TrimSpace(setting.AntomSettlementCurrency))
	if settlementCurrency != "" {
		if _, err := AntomMinorAmount(decimal.NewFromInt(1), settlementCurrency); err != nil {
			return nil, err
		}
	}
	return &AntomClient{
		ClientID: clientID, Sandbox: setting.AntomSandbox, gatewayURL: gateway,
		merchantPrivateKey: privateKey, antomPublicKey: publicKey,
		settlementCurrency: settlementCurrency,
		httpClient:         &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func parseAntomPrivateKey(value string) (*rsa.PrivateKey, error) {
	der, err := antomKeyDER(value)
	if err != nil {
		return nil, errors.New("invalid Antom merchant private key")
	}
	var key *rsa.PrivateKey
	if parsed, parseErr := x509.ParsePKCS8PrivateKey(der); parseErr == nil {
		key, _ = parsed.(*rsa.PrivateKey)
	} else {
		key, _ = x509.ParsePKCS1PrivateKey(der)
	}
	if key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, errors.New("Antom merchant key must be a valid RSA key of at least 2048 bits")
	}
	return key, nil
}

func parseAntomPublicKey(value string) (*rsa.PublicKey, error) {
	der, err := antomKeyDER(value)
	if err != nil {
		return nil, errors.New("invalid Antom public key")
	}
	var key *rsa.PublicKey
	if parsed, parseErr := x509.ParsePKIXPublicKey(der); parseErr == nil {
		key, _ = parsed.(*rsa.PublicKey)
	} else {
		key, _ = x509.ParsePKCS1PublicKey(der)
	}
	if key == nil || key.N.BitLen() < 2048 {
		return nil, errors.New("Antom public key must be an RSA key of at least 2048 bits")
	}
	return key, nil
}

// ValidateAntomOption validates one administrative update without mutating the
// live configuration. Empty credentials may disable the integration.
func ValidateAntomOption(key, value string) error {
	value = strings.TrimSpace(value)
	switch key {
	case "AntomGatewayUrl":
		switch strings.TrimRight(value, "/") {
		case "", "https://open-sea-global.alipay.com", "https://open-na-global.alipay.com", "https://open-de-global.alipay.com":
			return nil
		default:
			return errors.New("unsupported Antom regional gateway")
		}
	case "AntomClientId":
		if len(value) > 64 || strings.ContainsAny(value, "\r\n\t ") {
			return errors.New("invalid Antom client ID")
		}
	case "AntomMerchantPrivateKey":
		if value != "" {
			_, err := parseAntomPrivateKey(value)
			return err
		}
	case "AntomPublicKey":
		if value != "" {
			_, err := parseAntomPublicKey(value)
			return err
		}
	case "AntomCurrency", "AntomSettlementCurrency":
		if key == "AntomSettlementCurrency" && value == "" {
			return nil
		}
		_, err := AntomMinorAmount(decimal.NewFromInt(1), value)
		return err
	case "AntomUnitPrice":
		price, err := strconv.ParseFloat(value, 64)
		if err != nil || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return errors.New("Antom unit price must be finite and positive")
		}
	case "AntomMinTopUp":
		minimum, err := strconv.Atoi(value)
		if err != nil || minimum < 1 {
			return errors.New("Antom minimum top-up must be a positive integer")
		}
	case "AntomSandbox":
		if value != "true" && value != "false" {
			return errors.New("invalid Antom sandbox setting")
		}
	case "AntomNotifyUrl", "AntomReturnUrl":
		if value == "" {
			return nil
		}
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("invalid Antom callback or return URL")
		}
	}
	return nil
}

func antomKeyDER(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if block, rest := pem.Decode([]byte(value)); block != nil {
		if len(bytes.TrimSpace(rest)) != 0 {
			return nil, errors.New("unexpected data after key")
		}
		return block.Bytes, nil
	}
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value), ""))
}

func antomSignatureContent(method, path, clientID, timestamp, body string) string {
	return method + " " + path + "\n" + clientID + "." + timestamp + "." + body
}

func verifyAntomSignature(content, header string, key *rsa.PublicKey) error {
	parts := make(map[string]string, 3)
	for field := range strings.SplitSeq(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || parts[name] != "" {
			return errors.New("invalid Antom signature header")
		}
		parts[name] = strings.TrimSpace(value)
	}
	if parts["algorithm"] != "RSA256" || parts["keyVersion"] == "" || parts["signature"] == "" {
		return errors.New("unsupported Antom signature")
	}
	encoded, err := url.QueryUnescape(parts["signature"])
	if err != nil {
		return errors.New("invalid Antom signature encoding")
	}
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("invalid Antom signature encoding")
	}
	digest := sha256.Sum256([]byte(content))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature)
}

func (client *AntomClient) request(ctx context.Context, operation string, payload, result any) error {
	path := "/ams/api/v1/payments/" + operation
	if client.Sandbox {
		path = "/ams/sandbox/api/v1/payments/" + operation
	}
	body, err := common.Marshal(payload)
	if err != nil {
		return err
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	digest := sha256.Sum256([]byte(antomSignatureContent(http.MethodPost, path, client.ClientID, timestamp, string(body))))
	signature, err := rsa.SignPKCS1v15(rand.Reader, client.merchantPrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.gatewayURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("Client-Id", client.ClientID)
	req.Header.Set("Request-Time", timestamp)
	req.Header.Set("Signature", "algorithm=RSA256, keyVersion=1, signature="+url.QueryEscape(base64.StdEncoding.EncodeToString(signature)))
	response, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Antom request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Antom returned HTTP %d", response.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, antomMaxResponseBytes+1))
	if err != nil || len(responseBody) > antomMaxResponseBytes {
		return errors.New("invalid Antom response body")
	}
	responseTime := response.Header.Get("Response-Time")
	if response.Header.Get("Client-Id") != client.ClientID || responseTime == "" {
		return errors.New("Antom response identity is missing or mismatched")
	}
	content := antomSignatureContent(http.MethodPost, path, client.ClientID, responseTime, string(responseBody))
	if err := verifyAntomSignature(content, response.Header.Get("Signature"), client.antomPublicKey); err != nil {
		return errors.New("Antom response signature verification failed")
	}
	var envelope struct {
		Result struct {
			Status string `json:"resultStatus"`
			Code   string `json:"resultCode"`
		} `json:"result"`
	}
	if err := common.Unmarshal(responseBody, &envelope); err != nil {
		return errors.New("invalid Antom response JSON")
	}
	if envelope.Result.Status != "S" {
		return fmt.Errorf("Antom API did not succeed: %s", envelope.Result.Code)
	}
	return common.Unmarshal(responseBody, result)
}

type AntomSessionParams struct {
	PaymentRequestID string
	BuyerID          string
	Description      string
	NotifyURL        string
	ReturnURL        string
	Amount           AntomAmount
}

type AntomSession struct {
	CheckoutURL string `json:"normalUrl"`
}

func (client *AntomClient) CreatePaymentSession(ctx context.Context, params AntomSessionParams) (*AntomSession, error) {
	if params.PaymentRequestID == "" || len(params.PaymentRequestID) > 64 || params.BuyerID == "" || params.Description == "" {
		return nil, errors.New("invalid Antom order identifiers")
	}
	if _, err := AntomAmountMajor(params.Amount); err != nil {
		return nil, err
	}
	for _, address := range []string{params.NotifyURL, params.ReturnURL} {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Fragment != "" {
			return nil, errors.New("invalid Antom callback or return URL")
		}
	}
	payload := map[string]any{
		"productCode": "CASHIER_PAYMENT", "productScene": "CHECKOUT_PAYMENT",
		"paymentRequestId": params.PaymentRequestID, "paymentAmount": params.Amount,
		"paymentNotifyUrl": params.NotifyURL, "paymentRedirectUrl": params.ReturnURL,
		"paymentFactor": map[string]string{"captureMode": "AUTOMATIC"},
		"order": map[string]any{
			"referenceOrderId": params.PaymentRequestID, "orderDescription": params.Description,
			"orderAmount": params.Amount, "buyer": map[string]string{"referenceBuyerId": params.BuyerID},
		},
	}
	settlementCurrency := client.settlementCurrency
	if settlementCurrency == "" {
		settlementCurrency = params.Amount.Currency
	}
	payload["settlementStrategy"] = map[string]string{"settlementCurrency": settlementCurrency}
	var session AntomSession
	if err := client.request(ctx, "createPaymentSession", payload, &session); err != nil {
		return nil, err
	}
	u, err := url.Parse(session.CheckoutURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("Antom returned an invalid checkout URL")
	}
	return &session, nil
}

type AntomNotification struct {
	NotifyType       string `json:"notifyType"`
	PaymentRequestID string `json:"paymentRequestId"`
	PaymentID        string `json:"paymentId"`
}

func (client *AntomClient) VerifyNotification(method, path string, headers http.Header, body []byte) (*AntomNotification, error) {
	if method != http.MethodPost || headers.Get("Client-Id") != client.ClientID || headers.Get("Request-Time") == "" || len(body) > antomMaxResponseBytes {
		return nil, errors.New("invalid Antom notification headers or body")
	}
	content := antomSignatureContent(method, path, client.ClientID, headers.Get("Request-Time"), string(body))
	if err := verifyAntomSignature(content, headers.Get("Signature"), client.antomPublicKey); err != nil {
		return nil, errors.New("Antom notification signature verification failed")
	}
	var notification AntomNotification
	if err := common.Unmarshal(body, &notification); err != nil {
		return nil, errors.New("invalid Antom notification JSON")
	}
	if (notification.NotifyType != "PAYMENT_RESULT" && notification.NotifyType != "CAPTURE_RESULT") || (notification.PaymentRequestID == "" && notification.PaymentID == "") {
		return nil, errors.New("unsupported Antom notification")
	}
	// Authentic notifications trigger a signed inquiry; retries and old deliveries
	// are safe because fulfillment is bound to an immutable, idempotent order.
	return &notification, nil
}

type AntomPayment struct {
	PaymentRequestID string      `json:"paymentRequestId"`
	PaymentID        string      `json:"paymentId"`
	Status           string      `json:"paymentStatus"`
	Amount           AntomAmount `json:"paymentAmount"`
	PaymentMethod    string      `json:"paymentMethodType"`
	Transactions     []struct {
		Type   string      `json:"transactionType"`
		Status string      `json:"transactionStatus"`
		Amount AntomAmount `json:"transactionAmount"`
	} `json:"transactions"`
}

func (payment *AntomPayment) IsPaid() bool {
	if payment == nil || payment.Status != "SUCCESS" {
		return false
	}
	amount, err := AntomAmountMajor(payment.Amount)
	if err != nil {
		return false
	}
	// For card-backed methods SUCCESS can mean authorization only, even with
	// automatic capture. Fulfill only after the full capture is confirmed.
	switch strings.ReplaceAll(strings.ToUpper(payment.PaymentMethod), "_", "") {
	case "", "CARD", "APPLEPAY", "GOOGLEPAY":
		for _, transaction := range payment.Transactions {
			captured, err := AntomAmountMajor(transaction.Amount)
			if transaction.Type == "CAPTURE" && transaction.Status == "SUCCESS" && err == nil && transaction.Amount.Currency == payment.Amount.Currency && captured.Equal(amount) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (client *AntomClient) InquiryPayment(ctx context.Context, paymentRequestID, paymentID string) (*AntomPayment, error) {
	if paymentRequestID == "" && paymentID == "" {
		return nil, errors.New("Antom inquiry requires a payment identifier")
	}
	payload := map[string]string{}
	if paymentRequestID != "" {
		payload["paymentRequestId"] = paymentRequestID
	}
	if paymentID != "" {
		payload["paymentId"] = paymentID
	}
	var payment AntomPayment
	if err := client.request(ctx, "inquiryPayment", payload, &payment); err != nil {
		return nil, err
	}
	if payment.PaymentRequestID == "" || payment.PaymentID == "" || (paymentRequestID != "" && paymentRequestID != payment.PaymentRequestID) || (paymentID != "" && paymentID != payment.PaymentID) {
		return nil, errors.New("Antom inquiry payment identifiers do not match")
	}
	if _, err := AntomAmountMajor(payment.Amount); err != nil {
		return nil, err
	}
	return &payment, nil
}
