package controller

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanhpk/randstr"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func antomTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	var dialector gorm.Dialector = sqlite.Open(":memory:")
	if dsn := os.Getenv("ANTOM_TEST_MYSQL_DSN"); dsn != "" {
		dialector = mysql.Open(dsn)
	}
	if dsn := os.Getenv("ANTOM_TEST_POSTGRES_DSN"); dsn != "" {
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	if dialector.Name() == "sqlite" {
		sqlDB.SetMaxOpenConns(1)
	}
	oldDB, oldLogDB, oldRedis := model.DB, model.LOG_DB, common.RedisEnabled
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, model.LOG_DB, common.RedisEnabled = db, db, false
	common.SetDatabaseTypes(common.DatabaseType(dialector.Name()), common.DatabaseType(dialector.Name()))
	oldMaster := common.IsMasterNode
	common.IsMasterNode = false
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	common.IsMasterNode = oldMaster
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.TopUp{}, &model.AntomOrder{}, &model.SubscriptionOrder{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.Log{}, &model.Option{}))
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.RedisEnabled = oldDB, oldLogDB, oldRedis
		common.SetDatabaseTypes(oldMainType, oldLogType)
		require.NoError(t, sqlDB.Close())
	})
	return db
}

func antomTestUser(t *testing.T, db *gorm.DB) model.User {
	user := model.User{Username: "antom_" + randstr.Hex(12), AffCode: randstr.Hex(8), Quota: 20, Group: "default", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create(&user).Error)
	return user
}

func TestAntomManualSettlementSnapshot(t *testing.T) {
	db := antomTestDB(t)
	user := antomTestUser(t, db)
	trade := "antom_manual_" + randstr.Hex(12)
	topup := model.TopUp{UserId: user.Id, TradeNo: trade, Amount: 1, Money: 999, PaymentProvider: model.PaymentProviderAntom, PaymentMethod: model.PaymentMethodAntom, Status: common.TopUpStatusPending}
	snapshot := model.AntomOrder{UserId: user.Id, PaymentRequestId: trade, CreditedQuota: 123, Currency: "USD", AmountValue: "100", ClientId: "test"}
	require.NoError(t, model.CreateAntomTopUpWithSnapshot(&topup, &snapshot))
	require.NoError(t, model.ManualCompleteTopUp(trade, ""))
	require.NoError(t, model.ManualCompleteTopUp(trade, ""))
	require.NoError(t, db.First(&user, user.Id).Error)
	require.Equal(t, 143, user.Quota)
	row := model.TopUp{UserId: user.Id, TradeNo: trade + "_sub", Amount: 0, Money: 10, PaymentProvider: model.PaymentProviderStripe, Status: common.TopUpStatusPending}
	require.NoError(t, db.Create(&row).Error)
	require.Error(t, model.ManualCompleteTopUp(row.TradeNo, ""))
	require.NoError(t, db.First(&user, user.Id).Error)
	require.Equal(t, 143, user.Quota)
}

type antomTestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (transport antomTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	address := *req.URL
	address.Scheme, address.Host = transport.target.Scheme, transport.target.Host
	clone.URL = &address
	return transport.base.RoundTrip(clone)
}

type antomTestProvider struct {
	mu           sync.Mutex
	key          *rsa.PrivateKey
	payments     map[string]map[string]any
	corrupt      bool
	failCheckout bool
}

func antomTestSignature(key *rsa.PrivateKey, path, timestamp, body string) string {
	digest := sha256.Sum256([]byte("POST " + path + "\nantom-test." + timestamp + "." + body))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err)
	}
	return "algorithm=RSA256, keyVersion=1, signature=" + url.QueryEscape(base64.StdEncoding.EncodeToString(signature))
}

func antomProvider(t *testing.T) *antomTestProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	oldClient, oldPrivate, oldPublic, oldGateway, oldCurrency, oldNotify, oldReturn := setting.AntomClientId, setting.AntomMerchantPrivateKey, setting.AntomPublicKey, setting.AntomGatewayUrl, setting.AntomCurrency, setting.AntomNotifyUrl, setting.AntomReturnUrl
	oldSandbox, oldPrice, oldMin, oldQuota := setting.AntomSandbox, setting.AntomUnitPrice, setting.AntomMinTopUp, common.QuotaPerUnit
	oldPayment, oldDisplay := *operation_setting.GetPaymentSetting(), operation_setting.GetGeneralSetting().QuotaDisplayType
	setting.AntomClientId, setting.AntomSandbox, setting.AntomGatewayUrl = "antom-test", true, "https://open-sea-global.alipay.com"
	setting.AntomMerchantPrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	setting.AntomPublicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	setting.AntomCurrency, setting.AntomUnitPrice, setting.AntomMinTopUp, common.QuotaPerUnit = "USD", 1, 1, 100
	setting.AntomNotifyUrl, setting.AntomReturnUrl = "https://merchant.example/api/antom/webhook/test", "https://merchant.example/console/topup"
	operation_setting.GetPaymentSetting().ComplianceConfirmed = true
	operation_setting.GetPaymentSetting().ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{}
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD
	provider := &antomTestProvider{key: key, payments: map[string]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		encoded, err := url.QueryUnescape(strings.Split(r.Header.Get("Signature"), "signature=")[1])
		if err != nil {
			t.Error(err)
			return
		}
		signature, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Error(err)
			return
		}
		digest := sha256.Sum256([]byte("POST " + r.URL.Path + "\nantom-test." + r.Header.Get("Request-Time") + "." + string(body)))
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
			t.Error(err)
			w.WriteHeader(401)
			return
		}
		var request map[string]any
		if err := common.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		provider.mu.Lock()
		defer provider.mu.Unlock()
		response := map[string]any{"result": map[string]string{"resultCode": "SUCCESS", "resultStatus": "S"}}
		if strings.HasSuffix(r.URL.Path, "createPaymentSession") {
			id := request["paymentRequestId"].(string)
			provider.payments[id] = map[string]any{"paymentRequestId": id, "paymentId": "pay-" + id, "paymentStatus": "PROCESSING", "paymentAmount": request["paymentAmount"], "paymentMethodType": "CARD"}
			if provider.failCheckout {
				w.WriteHeader(503)
				return
			}
			response["normalUrl"] = "https://checkout.example/" + id
		} else {
			id, _ := request["paymentRequestId"].(string)
			if id == "" {
				id = strings.TrimPrefix(request["paymentId"].(string), "pay-")
			}
			for k, v := range provider.payments[id] {
				response[k] = v
			}
		}
		encodedBody, _ := common.Marshal(response)
		timestamp := time.Now().UTC().Format(time.RFC3339Nano)
		w.Header().Set("Client-Id", "antom-test")
		w.Header().Set("Response-Time", timestamp)
		signedBody := string(encodedBody)
		if provider.corrupt {
			signedBody += " "
		}
		w.Header().Set("Signature", antomTestSignature(key, r.URL.Path, timestamp, signedBody))
		_, _ = w.Write(encodedBody)
	}))
	target, _ := url.Parse(server.URL)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = antomTestTransport{target: target, base: oldTransport}
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		server.Close()
		setting.AntomClientId, setting.AntomMerchantPrivateKey, setting.AntomPublicKey, setting.AntomGatewayUrl, setting.AntomCurrency, setting.AntomNotifyUrl, setting.AntomReturnUrl = oldClient, oldPrivate, oldPublic, oldGateway, oldCurrency, oldNotify, oldReturn
		setting.AntomSandbox, setting.AntomUnitPrice, setting.AntomMinTopUp, common.QuotaPerUnit = oldSandbox, oldPrice, oldMin, oldQuota
		*operation_setting.GetPaymentSetting() = oldPayment
		operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplay
	})
	return provider
}

func antomCall(user int, method, path, body string, handler gin.HandlerFunc, params gin.Params, headers http.Header) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", user)
	c.Params = params
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		c.Request.Header[k] = v
	}
	handler(c)
	c.Writer.WriteHeaderNow()
	return recorder
}

func antomCheckout(t *testing.T, user int, handler gin.HandlerFunc, body string) string {
	t.Helper()
	response := antomCall(user, "POST", "/pay", body, handler, nil, nil)
	var result struct {
		Message string `json:"message"`
		Data    struct {
			Order string `json:"order_id"`
			URL   string `json:"checkout_url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result), response.Body.String())
	require.Equal(t, "success", result.Message, response.Body.String())
	require.Equal(t, "https://checkout.example/"+result.Data.Order, result.Data.URL)
	return result.Data.Order
}

func (p *antomTestProvider) update(id string, fields map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range fields {
		p.payments[id][k] = v
	}
}
func (p *antomTestProvider) notify(id, env string, valid bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"notifyType":"CAPTURE_RESULT","paymentId":%q}`, "pay-"+id)
	path := "/api/antom/webhook/" + env
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	signature := antomTestSignature(p.key, path, timestamp, body)
	if !valid {
		signature += "invalid"
	}
	return antomCall(0, "POST", path, body, AntomWebhook, gin.Params{{Key: "env", Value: env}}, http.Header{"Client-Id": {"antom-test"}, "Request-Time": {timestamp}, "Signature": {signature}})
}
func antomQuery(user int, id string) *httptest.ResponseRecorder {
	return antomCall(user, "GET", "/orders/"+id, "", GetAntomOrder, gin.Params{{Key: "trade_no", Value: id}}, nil)
}

func TestAntomSignedWalletLifecycle(t *testing.T) {
	db := antomTestDB(t)
	provider := antomProvider(t)
	user := antomTestUser(t, db)
	operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeTokens
	quote := antomCall(user.Id, "POST", "/amount", `{"amount":125}`, RequestAntomAmount, nil, nil)
	require.JSONEq(t, `{"message":"success","data":1.25}`, quote.Body.String())
	id := antomCheckout(t, user.Id, RequestAntomPay, `{"amount":125}`)
	snapshot := model.GetAntomOrderByTradeNo(id)
	require.Equal(t, 125, snapshot.CreditedQuota)
	require.Equal(t, "125", snapshot.AmountValue)
	require.Equal(t, http.StatusNotFound, antomQuery(user.Id+1, id).Code)
	require.Equal(t, http.StatusUnauthorized, provider.notify(id, "test", false).Code)
	require.Equal(t, http.StatusBadRequest, provider.notify(id, "prod", true).Code)
	provider.update(id, map[string]any{"paymentStatus": "FAIL"})
	require.Contains(t, antomQuery(user.Id, id).Body.String(), `"status":"pending"`)
	provider.update(id, map[string]any{"paymentStatus": "CANCELLED"})
	assert.Contains(t, antomQuery(user.Id, id).Body.String(), `"status":"pending"`)
	provider.update(id, map[string]any{"paymentStatus": "SUCCESS"})
	require.Contains(t, antomQuery(user.Id, id).Body.String(), `"status":"pending"`)
	amount := map[string]string{"currency": "USD", "value": "125"}
	provider.update(id, map[string]any{"transactions": []any{map[string]any{"transactionType": "CAPTURE", "transactionStatus": "SUCCESS", "transactionAmount": amount}}})
	provider.corrupt = true
	require.NotContains(t, antomQuery(user.Id, id).Body.String(), `"success":true`)
	provider.corrupt = false
	provider.update(id, map[string]any{"paymentAmount": map[string]string{"currency": "USD", "value": "126"}})
	require.Equal(t, http.StatusInternalServerError, provider.notify(id, "test", true).Code)
	provider.update(id, map[string]any{"paymentAmount": amount})
	// Neither changed pricing nor revocation of new purchases blocks fulfillment.
	setting.AntomUnitPrice, common.QuotaPerUnit = 9, 900
	operation_setting.GetPaymentSetting().ComplianceConfirmed = false
	require.Contains(t, antomCall(user.Id, "POST", "/pay", `{"amount":125}`, RequestAntomPay, nil, nil).Body.String(), `"message":"error"`)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var response *httptest.ResponseRecorder
			if i%2 == 0 {
				response = provider.notify(id, "test", true)
			} else {
				response = antomQuery(user.Id, id)
			}
			if response.Code != 200 || strings.Contains(response.Body.String(), `"success":false`) {
				t.Errorf("settlement failed: %d %s", response.Code, response.Body.String())
			}
		}(i)
	}
	wg.Wait()
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 145, user.Quota)
	assert.Equal(t, common.TopUpStatusSuccess, model.GetTopUpByTradeNo(id).Status)
}

func TestAntomSubscriptionAndCheckoutFailure(t *testing.T) {
	db := antomTestDB(t)
	provider := antomProvider(t)
	user := antomTestUser(t, db)
	plan := model.SubscriptionPlan{Title: "Antom test plan", PriceAmount: 3, Currency: "USD", Enabled: true, DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 1000}
	require.NoError(t, db.Create(&plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	t.Cleanup(func() { model.InvalidateSubscriptionPlanCache(plan.Id) })
	id := antomCheckout(t, user.Id, SubscriptionRequestAntomPay, fmt.Sprintf(`{"plan_id":%d}`, plan.Id))
	provider.update(id, map[string]any{"paymentStatus": "SUCCESS", "paymentMethodType": "ALIPAY_CN"})
	require.Equal(t, 200, provider.notify(id, "test", true).Code)
	require.Contains(t, antomQuery(user.Id, id).Body.String(), `"status":"success"`)
	var count int64
	require.NoError(t, db.Model(&model.UserSubscription{}).Where("user_id = ?", user.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 20, user.Quota)
	require.Error(t, model.ManualCompleteTopUp(id, ""))
	_, err := model.RechargeAntomTopUp(id, "")
	require.Error(t, err)
	// A provider timeout may still have created a payable session. Keep pending.
	provider.failCheckout = true
	response := antomCall(user.Id, "POST", "/pay", `{"amount":2}`, RequestAntomPay, nil, nil)
	require.Contains(t, response.Body.String(), `"message":"error"`)
	var pending model.TopUp
	require.NoError(t, db.Where("user_id = ? AND status = ?", user.Id, common.TopUpStatusPending).First(&pending).Error)
	provider.update(pending.TradeNo, map[string]any{"paymentStatus": "SUCCESS", "paymentMethodType": "ALIPAY_CN"})
	require.Equal(t, 200, provider.notify(pending.TradeNo, "test", true).Code)
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 220, user.Quota)
}

func TestAntomSnapshotRejectsWrongMerchantAndCapacity(t *testing.T) {
	db := antomTestDB(t)
	provider := antomProvider(t)
	user := antomTestUser(t, db)
	id := antomCheckout(t, user.Id, RequestAntomPay, `{"amount":1}`)
	provider.update(id, map[string]any{"paymentStatus": "SUCCESS", "paymentMethodType": "ALIPAY_CN"})
	snapshot := model.GetAntomOrderByTradeNo(id)
	client, err := service.NewAntomClient()
	require.NoError(t, err)
	payment, err := client.InquiryPayment(t.Context(), id, "")
	require.NoError(t, err)
	snapshot.ClientId = "other"
	_, err = settleAntomPayment(client, snapshot, payment, "")
	require.Error(t, err)
	require.NoError(t, db.Model(&user).Update("quota", common.MaxWalletQuota).Error)
	require.Equal(t, 500, provider.notify(id, "test", true).Code)
	require.Equal(t, common.TopUpStatusPending, model.GetTopUpByTradeNo(id).Status)
}

func TestAntomSettingsGuard(t *testing.T) {
	antomProvider(t)
	key := setting.AntomMerchantPrivateKey
	response := antomCall(1, "PUT", "/option", `{"key":"AntomMerchantPrivateKey","value":""}`, UpdateOption, nil, nil)
	require.Contains(t, response.Body.String(), `"success":true`)
	require.Equal(t, key, setting.AntomMerchantPrivateKey)
	response = antomCall(1, "PUT", "/option", `{"key":"AntomUnitPrice","value":"NaN"}`, UpdateOption, nil, nil)
	require.Contains(t, response.Body.String(), `"success":false`)
	require.Equal(t, 1.0, setting.AntomUnitPrice)
	oldOptions := common.OptionMap
	common.OptionMap = map[string]string{"AntomMerchantPrivateKey": key, "AntomPublicKey": setting.AntomPublicKey, "AntomClientId": "antom-test"}
	t.Cleanup(func() { common.OptionMap = oldOptions })
	response = antomCall(1, "GET", "/option", "", GetOptions, nil, nil)
	require.NotContains(t, response.Body.String(), "AntomMerchantPrivateKey")
	operation_setting.GetPaymentSetting().ComplianceConfirmed = false
	require.Contains(t, response.Body.String(), "AntomPublicKey")
	response = antomCall(1, "GET", "/topup/info", "", GetTopUpInfo, nil, nil)
	require.Contains(t, response.Body.String(), `"enable_antom_topup":false`)
	require.Contains(t, response.Body.String(), `"enable_antom_subscription":false`)
}

func TestAntomTopUpInfoMinimumMatchesAcceptedInput(t *testing.T) {
	db := antomTestDB(t)
	antomProvider(t)
	user := antomTestUser(t, db)
	common.QuotaPerUnit = 500000
	for _, tc := range []struct {
		display string
		minimum int
	}{
		{operation_setting.QuotaDisplayTypeUSD, 1},
		{operation_setting.QuotaDisplayTypeTokens, 500000},
	} {
		t.Run(tc.display, func(t *testing.T) {
			operation_setting.GetGeneralSetting().QuotaDisplayType = tc.display
			response := antomCall(user.Id, "GET", "/topup/info", "", GetTopUpInfo, nil, nil)
			var info struct {
				Data struct {
					Minimum    int                 `json:"antom_min_topup"`
					PayMethods []map[string]string `json:"pay_methods"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &info))
			assert.Equal(t, tc.minimum, info.Data.Minimum)
			found := false
			for _, method := range info.Data.PayMethods {
				if method["type"] == model.PaymentMethodAntom {
					found = true
					assert.Equal(t, fmt.Sprint(tc.minimum), method["min_topup"])
				}
			}
			require.True(t, found)
			quote := antomCall(user.Id, "POST", "/amount", fmt.Sprintf(`{"amount":%d}`, info.Data.Minimum), RequestAntomAmount, nil, nil)
			assert.JSONEq(t, `{"message":"success","data":1}`, quote.Body.String(), "the advertised minimum must be payable")
			below := antomCall(user.Id, "POST", "/amount", fmt.Sprintf(`{"amount":%d}`, info.Data.Minimum-1), RequestAntomAmount, nil, nil)
			assert.Contains(t, below.Body.String(), `"message":"error"`)
		})
	}
}
