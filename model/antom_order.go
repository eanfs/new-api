package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"gorm.io/gorm"
)

// Antom order kinds. The kind is part of the immutable snapshot and decides
// which business record a settlement applies to: wallet top-ups credit quota,
// subscription orders never do.
const (
	AntomOrderKindTopUp        = "topup"
	AntomOrderKindSubscription = "subscription"
)

var (
	ErrAntomOrderNotFound     = errors.New("antom order not found")
	ErrAntomOrderKindMismatch = errors.New("antom order kind mismatch")
)

// AntomOrder is the immutable per-payment snapshot binding an Antom checkout
// to its wallet TopUp row or SubscriptionOrder row. Currency, minor-unit
// amount, credited quota, owner, order kind, client id and sandbox flag are
// written once when the checkout session is created (in the same transaction
// as the business order) and must never be mutated afterwards. Settlement is
// only allowed after the provider's signed query result is validated against
// this snapshot.
type AntomOrder struct {
	Id               int    `json:"id" gorm:"primaryKey"`
	TradeNo          string `json:"trade_no" gorm:"unique;type:varchar(255);index"`
	PaymentRequestId string `json:"payment_request_id" gorm:"unique;type:varchar(128);index"`
	OrderKind        string `json:"order_kind" gorm:"type:varchar(16);index"`
	UserId           int    `json:"user_id" gorm:"index"`
	Currency         string `json:"currency" gorm:"type:varchar(8)"`
	AmountValue      string `json:"amount_value" gorm:"type:varchar(32)"`
	CreditedQuota    int    `json:"credited_quota"`
	ClientId         string `json:"client_id" gorm:"type:varchar(128)"`
	Sandbox          bool   `json:"sandbox"`
	PaymentId        string `json:"payment_id" gorm:"type:varchar(128);default:''"`
	CreateTime       int64  `json:"create_time"`
	CompleteTime     int64  `json:"complete_time"`
}

// CreateAntomTopUpWithSnapshot atomically creates the pending wallet TopUp
// row and its immutable Antom snapshot.
func CreateAntomTopUpWithSnapshot(topUp *TopUp, snapshot *AntomOrder) error {
	if topUp == nil || snapshot == nil {
		return errors.New("invalid antom topup order")
	}
	snapshot.TradeNo = topUp.TradeNo
	snapshot.OrderKind = AntomOrderKindTopUp
	if snapshot.UserId != topUp.UserId || topUp.PaymentProvider != PaymentProviderAntom || topUp.Status != common.TopUpStatusPending || snapshot.CreditedQuota <= 0 {
		return errors.New("invalid Antom wallet snapshot")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(topUp).Error; err != nil {
			return err
		}
		return tx.Create(snapshot).Error
	})
}

// CreateAntomSubscriptionOrderWithSnapshot atomically creates the pending
// subscription order and its immutable Antom snapshot.
func CreateAntomSubscriptionOrderWithSnapshot(order *SubscriptionOrder, snapshot *AntomOrder) error {
	if order == nil || snapshot == nil {
		return errors.New("invalid antom subscription order")
	}
	snapshot.TradeNo = order.TradeNo
	snapshot.OrderKind = AntomOrderKindSubscription
	if snapshot.UserId != order.UserId || order.PaymentProvider != PaymentProviderAntom || order.Status != common.TopUpStatusPending || snapshot.CreditedQuota != 0 {
		return errors.New("invalid Antom subscription snapshot")
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(order).Error; err != nil {
			return err
		}
		return tx.Create(snapshot).Error
	})
}

func GetAntomOrderByTradeNo(tradeNo string) *AntomOrder {
	if tradeNo == "" {
		return nil
	}
	var snapshot AntomOrder
	if err := DB.Where("trade_no = ?", tradeNo).First(&snapshot).Error; err != nil {
		return nil
	}
	return &snapshot
}

func GetAntomOrderByPaymentRequestId(paymentRequestID string) *AntomOrder {
	if paymentRequestID == "" {
		return nil
	}
	var snapshot AntomOrder
	if err := DB.Where("payment_request_id = ?", paymentRequestID).First(&snapshot).Error; err != nil {
		return nil
	}
	return &snapshot
}

// RecordAntomPaymentId best-effort stores the provider payment id on the
// snapshot. Failure is non-fatal: settlement correctness relies on the
// snapshot amount/currency fields, not on the payment id.
func RecordAntomPaymentId(tradeNo string, paymentId string) {
	if tradeNo == "" || paymentId == "" {
		return
	}
	if err := DB.Model(&AntomOrder{}).Where("trade_no = ?", tradeNo).
		Update("payment_id", paymentId).Error; err != nil {
		common.SysError("failed to record antom payment id: " + err.Error())
	}
}

// RechargeAntomTopUp atomically completes an Antom wallet top-up: order row
// lock, provider/kind/status validation, success update and quota credit all
// happen in one transaction, so concurrent/duplicate callbacks (including
// across instances) credit at most once. alreadyDone=true marks an idempotent
// replay of an already completed order.
//
// The credited quota comes exclusively from the immutable snapshot captured
// at checkout creation — it is never recomputed from current settings.
// Callers MUST have validated the signed provider query result against the
// snapshot before invoking this.
func RechargeAntomTopUp(tradeNo string, callerIp string) (alreadyDone bool, err error) {
	if tradeNo == "" {
		return false, errors.New("未提供支付单号")
	}

	var quotaToAdd int
	topUp := &TopUp{}
	err = DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("trade_no = ?", tradeNo).First(topUp).Error; err != nil {
			return ErrTopUpNotFound
		}
		if topUp.PaymentProvider != PaymentProviderAntom {
			return ErrPaymentMethodMismatch
		}
		snapshot := &AntomOrder{}
		if err := lockForUpdate(tx).Where("trade_no = ?", tradeNo).First(snapshot).Error; err != nil {
			return ErrAntomOrderNotFound
		}
		if snapshot.OrderKind != AntomOrderKindTopUp || snapshot.UserId != topUp.UserId {
			return ErrAntomOrderKindMismatch
		}
		if snapshot.CreditedQuota <= 0 {
			return ErrInvalidTopUpQuota
		}
		if topUp.Status == common.TopUpStatusSuccess {
			alreadyDone = true
			return nil
		}
		if topUp.Status != common.TopUpStatusPending {
			return ErrTopUpStatusInvalid
		}
		quotaToAdd = snapshot.CreditedQuota
		now := common.GetTimestamp()
		topUp.CompleteTime = now
		topUp.Status = common.TopUpStatusSuccess
		if err := tx.Save(topUp).Error; err != nil {
			return err
		}
		if snapshot.CompleteTime == 0 {
			snapshot.CompleteTime = now
			if err := tx.Save(snapshot).Error; err != nil {
				return err
			}
		}
		return creditTopUpQuota(tx, topUp.UserId, quotaToAdd, nil)
	})
	if err != nil {
		if !errors.Is(err, ErrTopUpNotFound) && !errors.Is(err, ErrPaymentMethodMismatch) &&
			!errors.Is(err, ErrTopUpStatusInvalid) && !errors.Is(err, ErrAntomOrderNotFound) &&
			!errors.Is(err, ErrAntomOrderKindMismatch) && !errors.Is(err, ErrInvalidTopUpQuota) {
			common.SysError("antom topup failed: " + err.Error())
		}
		return false, err
	}
	if alreadyDone {
		return true, nil
	}
	syncCreditUserQuotaCache(topUp.UserId, quotaToAdd, "antom topup")

	common.SysLog(fmt.Sprintf("Antom 充值成功 trade_no=%s user_id=%d quota_to_add=%d money=%.2f", topUp.TradeNo, topUp.UserId, quotaToAdd, topUp.Money))
	RecordTopupLog(topUp.UserId, fmt.Sprintf("使用在线充值成功，充值金额: %v，支付金额：%f", logger.LogQuota(quotaToAdd), topUp.Money), callerIp, topUp.PaymentMethod, PaymentProviderAntom)
	return false, nil
}
