package resource

import "time"

type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition reports one aspect of a resource's status, e.g. {Type: "Ready"}.
type Condition struct {
	Type             string
	Status           ConditionStatus
	Reason           string
	Message          string
	LastTransitionAt time.Time
}
