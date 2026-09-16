package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/thanhpk/randstr"
)

func antomError(c *gin.Context, err error) {
	c.JSON(http.StatusOK, gin.H{"message": "error", "data": err.Error()})
}

// Wallet credit is independent of checkout discounts and is captured exactly,
// including fractional quota units when the UI is displaying tokens.
func antomTopUpAmount(c *gin.Context) (int64, int, service.AntomAmount, error) {
	var req AmountRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		return 0, 0, service.AntomAmount{}, errors.New("invalid top-up amount")
	}
	units := decimal.NewFromInt(req.Amount)
	quota := units.Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	if common.QuotaPerUnit <= 0 {
		return 0, 0, service.AntomAmount{}, errors.New("invalid quota unit")
	}
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		quota = units
		units = units.Div(decimal.NewFromFloat(common.QuotaPerUnit))
	}
	if units.LessThan(decimal.NewFromInt(int64(setting.AntomMinTopUp))) {
		return 0, 0, service.AntomAmount{}, errors.New("amount below minimum top-up")
	}
	credited, err := validateCreditedQuota(quota)
	if err != nil {
		return 0, 0, service.AntomAmount{}, err
	}
	if err = model.ValidateTopUpQuotaCapacity(c.GetInt("id"), credited); err != nil {
		return 0, 0, service.AntomAmount{}, err
	}
	group, err := model.GetUserGroup(c.GetInt("id"), true)
	if err != nil {
		return 0, 0, service.AntomAmount{}, err
	}
	ratio := common.GetTopupGroupRatio(group)
	if ratio == 0 {
		ratio = 1
	}
	discount := 1.0
	if value := operation_setting.GetPaymentSetting().AmountDiscount[int(req.Amount)]; value > 0 {
		discount = value
	}
	amount, err := service.AntomMinorAmount(units.Mul(decimal.NewFromFloat(setting.AntomUnitPrice)).Mul(decimal.NewFromFloat(ratio)).Mul(decimal.NewFromFloat(discount)), setting.AntomCurrency)
	return units.IntPart(), credited, amount, err
}

func RequestAntomAmount(c *gin.Context) {
	if !isAntomTopUpEnabled() {
		antomError(c, errors.New("Antom top-up is not enabled"))
		return
	}
	_, _, amount, err := antomTopUpAmount(c)
	if err != nil {
		antomError(c, err)
		return
	}
	major, err := service.AntomAmountMajor(amount)
	if err != nil {
		antomError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": major.InexactFloat64()})
}

func antomURLs(tradeNo string) (string, string, error) {
	env := "prod"
	if setting.AntomSandbox {
		env = "test"
	}
	notify := strings.TrimSpace(setting.AntomNotifyUrl)
	if notify == "" {
		notify = strings.TrimRight(service.GetCallbackAddress(), "/") + "/api/antom/webhook/" + env
	}
	ret := strings.TrimSpace(setting.AntomReturnUrl)
	if ret == "" {
		ret = paymentReturnPath("/wallet")
	}
	parsed, err := url.Parse(ret)
	if err != nil {
		return "", "", err
	}
	query := parsed.Query()
	query.Set("antom_order", tradeNo)
	parsed.RawQuery = query.Encode()
	return notify, parsed.String(), nil
}

func createAntomSession(c *gin.Context, client *service.AntomClient, snapshot *model.AntomOrder, description, notify, ret string) {
	session, err := client.CreatePaymentSession(c.Request.Context(), service.AntomSessionParams{PaymentRequestID: snapshot.PaymentRequestId, BuyerID: strconv.Itoa(snapshot.UserId), Description: description, NotifyURL: notify, ReturnURL: ret, Amount: service.AntomAmount{Currency: snapshot.Currency, Value: snapshot.AmountValue}})
	if err != nil {
		common.SysError("Antom checkout failed: " + err.Error())
		antomError(c, errors.New("unable to create Antom checkout"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": gin.H{"checkout_url": session.CheckoutURL, "order_id": snapshot.TradeNo}})
}

func RequestAntomPay(c *gin.Context) {
	if !isAntomTopUpEnabled() {
		antomError(c, errors.New("Antom top-up is not enabled"))
		return
	}
	units, quota, amount, err := antomTopUpAmount(c)
	if err != nil {
		antomError(c, err)
		return
	}
	client, err := service.NewAntomClient()
	if err != nil {
		antomError(c, err)
		return
	}
	tradeNo := "ANTOM" + randstr.Hex(24)
	notify, ret, err := antomURLs(tradeNo)
	if err != nil {
		antomError(c, err)
		return
	}
	major, err := service.AntomAmountMajor(amount)
	if err != nil {
		antomError(c, err)
		return
	}
	snapshot := &model.AntomOrder{TradeNo: tradeNo, PaymentRequestId: tradeNo, UserId: c.GetInt("id"), Currency: amount.Currency, AmountValue: amount.Value, CreditedQuota: quota, ClientId: client.ClientID, Sandbox: client.Sandbox, CreateTime: common.GetTimestamp()}
	order := &model.TopUp{UserId: snapshot.UserId, Amount: units, Money: major.InexactFloat64(), TradeNo: tradeNo, PaymentMethod: model.PaymentMethodAntom, PaymentProvider: model.PaymentProviderAntom, CreateTime: snapshot.CreateTime, Status: common.TopUpStatusPending}
	if err := model.CreateAntomTopUpWithSnapshot(order, snapshot); err != nil {
		antomError(c, err)
		return
	}
	createAntomSession(c, client, snapshot, "Wallet top-up", notify, ret)
}

func SubscriptionRequestAntomPay(c *gin.Context) {
	if !isAntomSubscriptionEnabled() {
		antomError(c, errors.New("Antom subscription purchase is not enabled"))
		return
	}
	var req struct {
		PlanId int `json:"plan_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.PlanId <= 0 {
		antomError(c, errors.New("invalid plan"))
		return
	}
	plan, err := model.GetSubscriptionPlanById(req.PlanId)
	if err != nil {
		antomError(c, err)
		return
	}
	if !plan.Enabled || plan.PriceAmount <= 0 {
		antomError(c, errors.New("plan is not available for purchase"))
		return
	}
	if plan.MaxPurchasePerUser > 0 {
		count, err := model.CountUserSubscriptionsByPlan(c.GetInt("id"), plan.Id)
		if err != nil {
			antomError(c, err)
			return
		}
		if count >= int64(plan.MaxPurchasePerUser) {
			antomError(c, errors.New("plan purchase limit reached"))
			return
		}
	}
	amount, err := service.AntomMinorAmount(decimal.NewFromFloat(plan.PriceAmount), plan.Currency)
	if err != nil {
		antomError(c, err)
		return
	}
	client, err := service.NewAntomClient()
	if err != nil {
		antomError(c, err)
		return
	}
	tradeNo := "ANTOMSUB" + randstr.Hex(24)
	notify, ret, err := antomURLs(tradeNo)
	if err != nil {
		antomError(c, err)
		return
	}
	snapshot := &model.AntomOrder{TradeNo: tradeNo, PaymentRequestId: tradeNo, UserId: c.GetInt("id"), Currency: amount.Currency, AmountValue: amount.Value, ClientId: client.ClientID, Sandbox: client.Sandbox, CreateTime: common.GetTimestamp()}
	order := &model.SubscriptionOrder{UserId: snapshot.UserId, PlanId: plan.Id, Money: plan.PriceAmount, TradeNo: tradeNo, PaymentMethod: model.PaymentMethodAntom, PaymentProvider: model.PaymentProviderAntom, CreateTime: snapshot.CreateTime, Status: common.TopUpStatusPending}
	if err := model.CreateAntomSubscriptionOrderWithSnapshot(order, snapshot); err != nil {
		antomError(c, err)
		return
	}
	createAntomSession(c, client, snapshot, plan.Title, notify, ret)
}

func settleAntomPayment(client *service.AntomClient, snapshot *model.AntomOrder, payment *service.AntomPayment, ip string) (string, error) {
	if snapshot == nil || snapshot.ClientId != client.ClientID || snapshot.Sandbox != client.Sandbox || payment.PaymentRequestID != snapshot.PaymentRequestId || payment.PaymentID == "" || (snapshot.PaymentId != "" && snapshot.PaymentId != payment.PaymentID) || payment.Amount.Currency != snapshot.Currency || payment.Amount.Value != snapshot.AmountValue {
		return "", errors.New("Antom payment does not match order snapshot")
	}
	status := "pending"
	switch snapshot.OrderKind {
	case model.AntomOrderKindTopUp:
		order := model.GetTopUpByTradeNo(snapshot.TradeNo)
		if order == nil || order.UserId != snapshot.UserId || order.PaymentProvider != model.PaymentProviderAntom {
			return "", errors.New("invalid Antom wallet order")
		}
		status = order.Status
	case model.AntomOrderKindSubscription:
		order := model.GetSubscriptionOrderByTradeNo(snapshot.TradeNo)
		if order == nil || order.UserId != snapshot.UserId || order.PaymentProvider != model.PaymentProviderAntom {
			return "", errors.New("invalid Antom subscription order")
		}
		status = order.Status
	default:
		return "", errors.New("invalid Antom order kind")
	}
	if status == common.TopUpStatusSuccess {
		return "success", nil
	}
	if payment.IsPaid() {
		var err error
		if snapshot.OrderKind == model.AntomOrderKindTopUp {
			_, err = model.RechargeAntomTopUp(snapshot.TradeNo, ip)
		} else {
			err = model.CompleteSubscriptionOrder(snapshot.TradeNo, "", model.PaymentProviderAntom, model.PaymentMethodAntom)
		}
		if err != nil {
			return "", err
		}
		model.RecordAntomPaymentId(snapshot.TradeNo, payment.PaymentID)
		return "success", nil
	}
	// A failed or cancelled attempt need not close the hosted checkout session.
	// Keep the order payable until a later signed inquiry confirms payment.
	return status, nil
}

func inquiryAntomOrder(ctx context.Context, client *service.AntomClient, snapshot *model.AntomOrder, ip string) (string, error) {
	if snapshot.ClientId != client.ClientID || snapshot.Sandbox != client.Sandbox {
		return "", errors.New("Antom merchant or environment changed")
	}
	payment, err := client.InquiryPayment(ctx, snapshot.PaymentRequestId, "")
	if err != nil {
		return "", err
	}
	return settleAntomPayment(client, snapshot, payment, ip)
}

func GetAntomOrder(c *gin.Context) {
	snapshot := model.GetAntomOrderByTradeNo(c.Param("trade_no"))
	if snapshot == nil || snapshot.UserId != c.GetInt("id") {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "order not found"})
		return
	}
	client, err := service.NewAntomClient()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	status, err := inquiryAntomOrder(c.Request.Context(), client, snapshot, c.ClientIP())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"status": status})
}

func AntomWebhook(c *gin.Context) {
	client, err := service.NewAntomClient()
	if err != nil || !isAntomWebhookEnabled() {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	env := "prod"
	if client.Sandbox {
		env = "test"
	}
	if c.Param("env") != env {
		c.Status(http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		c.Status(http.StatusBadRequest)
		return
	}
	notification, err := client.VerifyNotification(c.Request.Method, c.Request.URL.RequestURI(), c.Request.Header, body)
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	if notification.NotifyType != "PAYMENT_RESULT" && notification.NotifyType != "CAPTURE_RESULT" {
		c.JSON(http.StatusOK, gin.H{"result": gin.H{"resultCode": "SUCCESS", "resultStatus": "S", "resultMessage": "success"}})
		return
	}
	payment, err := client.InquiryPayment(c.Request.Context(), notification.PaymentRequestID, notification.PaymentID)
	if err == nil {
		snapshot := model.GetAntomOrderByPaymentRequestId(payment.PaymentRequestID)
		_, err = settleAntomPayment(client, snapshot, payment, c.ClientIP())
	}
	if err != nil {
		common.SysError(fmt.Sprintf("Antom notification settlement failed: %v", err))
		c.Status(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, gin.H{"result": gin.H{"resultCode": "SUCCESS", "resultStatus": "S", "resultMessage": "success"}})
}
