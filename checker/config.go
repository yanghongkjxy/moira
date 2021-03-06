package checker

import "time"

// Config represent checker config
type Config struct {
	Enabled              bool
	NoDataCheckInterval  time.Duration
	CheckInterval        time.Duration
	MetricsTTL           int64
	StopCheckingInterval int64
	MaxParallelChecks    int
	LogFile              string
	LogLevel             string
}
