package escrow

import (
	"fmt"
	"math/big"
	"strings"
)

type lockingPrepaidService struct {
	storage        TypedAtomicStorage
	validator      *PrePaidPaymentValidator
	replicaGroupID func() ([32]byte, error)
}

func NewPrePaidService(
	storage TypedAtomicStorage,
	prepaidValidator *PrePaidPaymentValidator, groupIdReader func() ([32]byte, error)) PrePaidService {
	return &lockingPrepaidService{
		storage:        storage,
		validator:      prepaidValidator,
		replicaGroupID: groupIdReader,
	}
}

func (h *lockingPrepaidService) ListPrePaidUsers() (users []*PrePaidData, err error) {
	data, err := h.storage.GetAll()
	return data.([]*PrePaidData), err
}

//Defines the condition that needs to be met, it generates the respective typed Data when
//conditions are satisfied, you define your own validations in here
//It takes in the latest typed values read.
type ConditionFunc func(params ...interface{}) ([]*TypedKeyValueData, error)

func (h *lockingPrepaidService) UpdateUsage(channelId *big.Int, revisedAmount *big.Int, updateUsageType string) (err error) {
	var conditionFunc ConditionFunc = nil

	switch updateUsageType {
	case USED_AMOUNT:
		conditionFunc = IncrementUsedAmount

	case PLANNED_AMOUNT:
		conditionFunc = IncrementPlannedAmount

	case REFUND_AMOUNT:
		conditionFunc = IncrementRefundAmount

	default:
		return fmt.Errorf("Unknow Update type %v", updateUsageType)
	}

	typedUpdateFunc := func(conditionValues []*TypedKeyValueData) (update []*TypedKeyValueData, err error) {

		var newValues interface{}
		if newValues, err = conditionFunc(conditionValues, revisedAmount); err != nil {
			return nil, err
		}

		return BuildOldAndNewValuesForCAS(newValues)
	}

	request := TypedCASRequest{
		Update:                  typedUpdateFunc,
		RetryTillSuccessOrError: true,
		ConditionKeys:           getAllKeys(channelId),
	}
	ok, err := h.storage.ExecuteTransaction(request)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Error in executing ExecuteTransaction for usage type"+
			"  %v on channel %v ", updateUsageType, channelId)
	}
	return nil
}

func getAllKeys(channelId *big.Int) []string {
	keys := make([]string, 3)
	for i, typ := range []string{REFUND_AMOUNT, PLANNED_AMOUNT, USED_AMOUNT} {
		keys[i] = channelId.String() + "/" + typ
	}
	return keys
}

var (
	//this function will be used to read typed data ,convert it in to a business structure
	//on which validations can be easily performed and return back the business structure.
	convertTypedDataToPrePaidUsage = func(latestReadData interface{}) (new interface{}, err error) {
		data := latestReadData.([]*TypedKeyValueData)
		usageData := &PrePaidUsageData{PlannedAmount: big.NewInt(0),
			UsedAmount: big.NewInt(0), FailedAmount: big.NewInt(0)}
		for _, usageType := range data {
			key := usageType.Key.(*PrePaidDataKey)
			data := usageType.Value.(*PrePaidData)
			usageData.ChannelID = key.ChannelID
			if strings.Compare(key.UsageType, USED_AMOUNT) == 0 {
				usageData.UsedAmount = data.Amount
			} else if strings.Compare(key.UsageType, PLANNED_AMOUNT) == 0 {
				usageData.PlannedAmount = data.Amount
			} else if strings.Compare(key.UsageType, REFUND_AMOUNT) == 0 {
				usageData.FailedAmount = data.Amount
			} else {
				return nil, fmt.Errorf("Unknown Usage Type %v", key.UsageType)
			}
		}
		return usageData, nil
	}
)

func BuildOldAndNewValuesForCAS(params ...interface{}) (newValues []*TypedKeyValueData, err error) {
	if len(params) == 0 {
		return nil, fmt.Errorf("No parameters passed for the Action function")
	}
	data := params[0].(*PrePaidUsageData)
	if data == nil {
		return nil, fmt.Errorf("Expected PrePaidUsageData in Params as the first parmeter")
	}
	updateUsageData := &PrePaidData{}
	updateUsageKey := &PrePaidDataKey{ChannelID: data.ChannelID, UsageType: data.UpdateUsageType}
	if amt, err := data.GetAmountForUsageType(); err != nil {
		return nil, err
	} else {
		updateUsageData.Amount = amt
	}
	newValue := &TypedKeyValueData{Key: updateUsageKey, Value: updateUsageData}
	newValues = make([]*TypedKeyValueData, 0)
	newValues = append(newValues, newValue)

	return newValues, nil
}

var (
	IncrementUsedAmount ConditionFunc = func(params ...interface{}) (newValues []*TypedKeyValueData, err error) {
		data := params[0].([]*TypedKeyValueData)
		if len(params) == 0 {
			return nil, fmt.Errorf("You need to pass the Price ")
		}
		businessObject, err := convertTypedDataToPrePaidUsage(data)
		if err != nil {
			return nil, err
		}
		oldState := businessObject.(*PrePaidUsageData)

		newState := oldState.Clone()
		usageKey := &PrePaidDataKey{UsageType: USED_AMOUNT, ChannelID: oldState.ChannelID}
		updateDetails(newState, usageKey, params[0].(*PrePaidData))
		if newState.UsedAmount.Cmp(oldState.PlannedAmount.Add(oldState.PlannedAmount, oldState.FailedAmount)) > 0 {
			return nil, fmt.Errorf("Usage Exceeded on channel %v", oldState.ChannelID)
		}
		return BuildOldAndNewValuesForCAS(newState)

	}
	//Make sure you update the planned amount ONLY when the new value is greater than what was last persisted
	IncrementPlannedAmount ConditionFunc = func(params ...interface{}) (newValues []*TypedKeyValueData, err error) {
		data := params[0].([]*PrePaidData)
		if len(params) == 0 {
			return nil, fmt.Errorf("You need to pass the Price and the Channel Id ")
		}
		businessObject, err := convertTypedDataToPrePaidUsage(data)
		if err != nil {
			return nil, err
		}
		oldState := businessObject.(*PrePaidUsageData)
		newState := oldState.Clone()
		usageKey := &PrePaidDataKey{UsageType: PLANNED_AMOUNT, ChannelID: oldState.ChannelID}
		updateDetails(newState, usageKey, params[0].(*PrePaidData))
		if newState.PlannedAmount.Cmp(oldState.PlannedAmount) < 0 {
			return nil, fmt.Errorf("A revised higher planned amount has been signed "+
				"already for %v on channel %v", oldState.PlannedAmount, oldState.ChannelID)
		}
		return BuildOldAndNewValuesForCAS(newState)

	}
	//If there is no refund amount yet, put it , else add latest value in DB with the additional refund to be done
	IncrementRefundAmount ConditionFunc = func(params ...interface{}) (newValues []*TypedKeyValueData, err error) {
		data := params[0].([]*PrePaidData)
		if len(params) == 0 {
			return nil, fmt.Errorf("You need to pass the Price ")
		}
		businessObject, err := convertTypedDataToPrePaidUsage(data)
		if err != nil {
			return nil, err
		}
		newState := businessObject.(*PrePaidUsageData)
		usageKey := &PrePaidDataKey{UsageType: PLANNED_AMOUNT, ChannelID: newState.ChannelID}
		updateDetails(newState, usageKey, params[0].(*PrePaidData))
		return BuildOldAndNewValuesForCAS(newState)

	}
)

func updateDetails(usageData *PrePaidUsageData, key *PrePaidDataKey, details *PrePaidData) {
	usageData.ChannelID = key.ChannelID
	usageData.UpdateUsageType = key.UsageType
	switch key.UsageType {
	case USED_AMOUNT:
		usageData.UsedAmount.Add(details.Amount, usageData.UsedAmount)
	case PLANNED_AMOUNT:
		usageData.PlannedAmount.Add(details.Amount, usageData.PlannedAmount)
	case REFUND_AMOUNT:
		usageData.FailedAmount.Add(details.Amount, usageData.FailedAmount)
	}
}
