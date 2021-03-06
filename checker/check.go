package checker

import (
	"fmt"

	"github.com/moira-alert/moira"
	"github.com/moira-alert/moira/target"
)

var checkPointGap int64 = 120

// ErrTriggerHasNoTimeSeries used if trigger no metrics
var ErrTriggerHasNoTimeSeries = fmt.Errorf("Trigger has no metrics")

// ErrTriggerHasOnlyWildcards used if trigger has only wildcard metrics
var ErrTriggerHasOnlyWildcards = fmt.Errorf("Trigger has only wildcards")

// ErrTriggerHasSameTimeSeriesNames used if trigger has two timeseries with same name
var ErrTriggerHasSameTimeSeriesNames = fmt.Errorf("Trigger has same timeseries names")

// Check handle trigger and last check and write new state of trigger, if state were change then write new NotificationEvent
func (triggerChecker *TriggerChecker) Check() error {
	triggerChecker.Logger.Debugf("Checking trigger %s", triggerChecker.TriggerID)
	checkData, err := triggerChecker.handleTrigger()
	if err != nil {
		checkData, err = triggerChecker.handleErrorCheck(checkData, err)
		if err != nil {
			return err
		}
	}
	checkData.UpdateScore()
	return triggerChecker.Database.SetTriggerLastCheck(triggerChecker.TriggerID, &checkData)
}

func (triggerChecker *TriggerChecker) handleTrigger() (moira.CheckData, error) {
	lastMetrics := make(map[string]moira.MetricState, len(triggerChecker.lastCheck.Metrics))
	for k, v := range triggerChecker.lastCheck.Metrics {
		lastMetrics[k] = v
	}
	checkData := moira.CheckData{
		Metrics:   lastMetrics,
		State:     OK,
		Timestamp: triggerChecker.Until,
		Score:     triggerChecker.lastCheck.Score,
	}

	triggerTimeSeries, metrics, err := triggerChecker.getTimeSeries(triggerChecker.From, triggerChecker.Until)
	if err != nil {
		return checkData, err
	}

	triggerChecker.cleanupMetricsValues(metrics, triggerChecker.Until)

	if len(triggerTimeSeries.Main) == 0 {
		return checkData, ErrTriggerHasNoTimeSeries
	}

	if triggerTimeSeries.hasOnlyWildcards() {
		return checkData, ErrTriggerHasOnlyWildcards
	}

	timeSeriesNamesMap := make(map[string]bool, len(triggerTimeSeries.Main))
	var checkingError error

	for _, timeSeries := range triggerTimeSeries.Main {
		triggerChecker.Logger.Debugf("[TriggerID:%s] Checking timeSeries %s: %v", triggerChecker.TriggerID, timeSeries.Name, timeSeries.Values)
		triggerChecker.Logger.Debugf("[TriggerID:%s][TimeSeries:%s] Checking interval: %v - %v (%vs), step: %v", triggerChecker.TriggerID, timeSeries.Name, timeSeries.StartTime, timeSeries.StopTime, timeSeries.StepTime, timeSeries.StopTime-timeSeries.StartTime)

		if _, ok := timeSeriesNamesMap[timeSeries.Name]; ok {
			checkingError = ErrTriggerHasSameTimeSeriesNames
			triggerChecker.Logger.Infof("[TriggerID:%s][TimeSeries:%s] Trigger has same timeseries names", triggerChecker.TriggerID, timeSeries.Name)
			continue
		}
		timeSeriesNamesMap[timeSeries.Name] = true
		metricState, needToDeleteMetric, err := triggerChecker.checkTimeSeries(timeSeries, triggerTimeSeries)
		if needToDeleteMetric {
			triggerChecker.Logger.Infof("[TriggerID:%s] Remove metric: '%s'", triggerChecker.TriggerID, timeSeries.Name)
			delete(checkData.Metrics, timeSeries.Name)
			err = triggerChecker.Database.RemovePatternsMetrics(triggerChecker.trigger.Patterns)
		} else {
			checkData.Metrics[timeSeries.Name] = metricState
		}
		if err != nil {
			return checkData, err
		}
	}
	return checkData, checkingError
}

func (triggerChecker *TriggerChecker) checkTimeSeries(timeSeries *target.TimeSeries, triggerTimeSeries *triggerTimeSeries) (lastState moira.MetricState, needToDeleteMetric bool, err error) {
	lastState = triggerChecker.lastCheck.GetOrCreateMetricState(timeSeries.Name, int64(timeSeries.StartTime-3600))
	metricStates, err := triggerChecker.getTimeSeriesStepsStates(triggerTimeSeries, timeSeries, lastState)
	if err != nil {
		return
	}
	for _, currentState := range metricStates {
		lastState, err = triggerChecker.compareStates(timeSeries.Name, currentState, lastState)
		if err != nil {
			return
		}
	}
	needToDeleteMetric, noDataState := triggerChecker.checkForNoData(timeSeries, lastState)
	if needToDeleteMetric {
		return
	}
	if noDataState != nil {
		lastState, err = triggerChecker.compareStates(timeSeries.Name, *noDataState, lastState)
	}
	return
}

func (triggerChecker *TriggerChecker) handleErrorCheck(checkData moira.CheckData, checkingError error) (moira.CheckData, error) {
	if checkingError == ErrTriggerHasNoTimeSeries {
		triggerChecker.Logger.Debugf("Trigger %s: %s", triggerChecker.TriggerID, checkingError.Error())
		checkData.State = NODATA
		checkData.Message = "Trigger has no metrics, check your target"
		if triggerChecker.ttl == 0 {
			return checkData, nil
		}
		return triggerChecker.compareChecks(checkData)
	}
	if checkingError == ErrTriggerHasOnlyWildcards {
		triggerChecker.Logger.Debugf("Trigger %s: %s", triggerChecker.TriggerID, checkingError.Error())
		if len(checkData.Metrics) == 0 {
			checkData.State = NODATA
			checkData.Message = "Trigger never received metrics"
			if triggerChecker.ttl == 0 || triggerChecker.ttlState == DEL {
				return checkData, nil
			}
			return triggerChecker.compareChecks(checkData)
		}
		return checkData, nil
	}
	if _, ok := checkingError.(target.ErrUnknownFunction); ok {
		triggerChecker.Logger.Warningf("Trigger %s: %s", triggerChecker.TriggerID, checkingError.Error())
		checkData.State = EXCEPTION
		checkData.Message = checkingError.Error()
	} else if checkingError == ErrTriggerHasSameTimeSeriesNames {
		checkData.State = EXCEPTION
		checkData.Message = "Trigger has same timeseries names"
	} else {
		triggerChecker.Metrics.CheckError.Mark(1)
		triggerChecker.Logger.Errorf("Trigger %s check failed: %s", triggerChecker.TriggerID, checkingError.Error())
		checkData.State = EXCEPTION
	}
	return triggerChecker.compareChecks(checkData)
}

func (triggerChecker *TriggerChecker) checkForNoData(timeSeries *target.TimeSeries, metricLastState moira.MetricState) (bool, *moira.MetricState) {
	if triggerChecker.ttl == 0 {
		return false, nil
	}
	lastCheckTimeStamp := triggerChecker.lastCheck.Timestamp

	if metricLastState.Timestamp+triggerChecker.ttl >= lastCheckTimeStamp {
		return false, nil
	}
	triggerChecker.Logger.Debugf("[TriggerID:%s][TimeSeries:%s] Metric TTL expired for state %v", triggerChecker.TriggerID, timeSeries.Name, metricLastState)
	if triggerChecker.ttlState == DEL && metricLastState.EventTimestamp != 0 {
		return true, nil
	}
	return false, &moira.MetricState{
		State:       toMetricState(triggerChecker.ttlState),
		Timestamp:   lastCheckTimeStamp - triggerChecker.ttl,
		Value:       nil,
		Maintenance: metricLastState.Maintenance,
		Suppressed:  metricLastState.Suppressed,
	}
}

func (triggerChecker *TriggerChecker) getTimeSeriesStepsStates(triggerTimeSeries *triggerTimeSeries, timeSeries *target.TimeSeries, metricLastState moira.MetricState) ([]moira.MetricState, error) {
	startTime := int64(timeSeries.StartTime)
	stepTime := int64(timeSeries.StepTime)

	checkPoint := metricLastState.GetCheckPoint(checkPointGap)
	triggerChecker.Logger.Debugf("[TriggerID:%s][TimeSeries:%s] Checkpoint: %v", triggerChecker.TriggerID, timeSeries.Name, checkPoint)

	metricStates := make([]moira.MetricState, 0)

	for valueTimestamp := startTime; valueTimestamp < triggerChecker.Until+stepTime; valueTimestamp += stepTime {
		metricNewState, err := triggerChecker.getTimeSeriesState(triggerTimeSeries, timeSeries, metricLastState, valueTimestamp, checkPoint)
		if err != nil {
			return nil, err
		}
		if metricNewState == nil {
			continue
		}
		metricLastState = *metricNewState
		metricStates = append(metricStates, *metricNewState)
	}
	return metricStates, nil
}

func (triggerChecker *TriggerChecker) getTimeSeriesState(triggerTimeSeries *triggerTimeSeries, timeSeries *target.TimeSeries, lastState moira.MetricState, valueTimestamp, checkPoint int64) (*moira.MetricState, error) {
	if valueTimestamp <= checkPoint {
		return nil, nil
	}
	triggerExpression, noEmptyValues := triggerTimeSeries.getExpressionValues(timeSeries, valueTimestamp)
	if !noEmptyValues {
		return nil, nil
	}
	triggerChecker.Logger.Debugf("[TriggerID:%s][TimeSeries:%s] Values for ts %v: MainTargetValue: %v, additionalTargetValues: %v", triggerChecker.TriggerID, timeSeries.Name, valueTimestamp, triggerExpression.MainTargetValue, triggerExpression.AdditionalTargetsValues)

	triggerExpression.WarnValue = triggerChecker.trigger.WarnValue
	triggerExpression.ErrorValue = triggerChecker.trigger.ErrorValue
	triggerExpression.PreviousState = lastState.State
	triggerExpression.Expression = triggerChecker.trigger.Expression

	expressionState, err := triggerExpression.Evaluate()
	if err != nil {
		return nil, err
	}

	return &moira.MetricState{
		State:       expressionState,
		Timestamp:   valueTimestamp,
		Value:       &triggerExpression.MainTargetValue,
		Maintenance: lastState.Maintenance,
		Suppressed:  lastState.Suppressed,
	}, nil
}

func (triggerChecker *TriggerChecker) cleanupMetricsValues(metrics []string, until int64) {
	if len(metrics) > 0 {
		if err := triggerChecker.Database.RemoveMetricsValues(metrics, until-triggerChecker.Config.MetricsTTL); err != nil {
			triggerChecker.Logger.Error(err.Error())
		}
	}
}
