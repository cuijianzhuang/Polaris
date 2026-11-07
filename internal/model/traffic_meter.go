package model

import "time"

// TrafficMeter describes evidence, not a guarantee of lossless billing.
// Absent reports from older agents must remain unknown.
type TrafficMeter struct {
	Mode                string     `json:"mode"`
	IntervalSeconds     int        `json:"intervalSeconds"`
	LastSuccess         *time.Time `json:"lastSuccess,omitempty"`
	LastAttempt         *time.Time `json:"lastAttempt,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
}
