package backend

import (
	"errors"
	"fmt"
	"time"
)

type Error struct{ Code int }

func (e *Error) Error() string     { return fmt.Sprintf("native operation failed (%d)", e.Code) }
func IsStatus(e error, n int) bool { var x *Error; return errors.As(e, &x) && x.Code == n }

type Script struct {
	Content string            `json:"script_content"`
	Env     map[string]string `json:"env"`
}
type Spec struct {
	Name          string            `json:"name"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	Runtime       string            `json:"runtime"`
	Image         string            `json:"image"`
	CPU           int64             `json:"cpu"`
	Memory        int64             `json:"memory"`
	Headless      bool              `json:"headless"`
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	RestartPolicy string            `json:"restart_policy"`
	Resources     map[string]int64  `json:"resources"`
	Labels        map[string]string `json:"labels"`
	StartupScript Script            `json:"startup_script"`
	Suspendable   bool              `json:"suspendable,omitempty"`
}
type Condition struct {
	Type  string `json:"type"`
	State string `json:"state"`
}
type VM struct {
	Conditions []Condition `json:"conditions"`
	Spec
	UID                string    `json:"uid"`
	Status             string    `json:"status"`
	StatusMessage      string    `json:"status_message"`
	Worker             string    `json:"worker"`
	CreatedAt          time.Time `json:"createdAt"`
	StartedAt          time.Time `json:"startedAt"`
	PowerState         string    `json:"powerState,omitempty"`
	LocalName          string    `json:"localName,omitempty"`
	Generation         uint64    `json:"generation"`
	ObservedGeneration uint64    `json:"observedGeneration"`
}

func (v VM) Stopped() bool {
	for _, c := range v.Conditions {
		if c.Type == "running" {
			return c.State == "false"
		}
	}
	return false
}

type Event struct {
	Message string `json:"message"`
	Payload string `json:"payload"`
}
