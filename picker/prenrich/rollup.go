package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// jqStr is jq's `.key // ""` for a string field: null, false and absent all
// fall through, and an empty string does not.
func jqStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func jqPresent(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	b, isBool := v.(bool)
	return !isBool || b
}

// collapseRollup maps a gh statusCheckRollup array (CheckRun | StatusContext)
// to none|failure|pending|success, plus "<finished>/<total>" while pending.
func collapseRollup(raw string) (state, progress string) {
	var checks []map[string]any
	if json.Unmarshal([]byte(raw), &checks) != nil || len(checks) == 0 {
		return "none", ""
	}
	pendingCount := 0
	failed := false
	for _, c := range checks {
		verdict := "conclusion"
		if !jqPresent(c, "conclusion") {
			verdict = "state"
		}
		switch strings.ToUpper(jqStr(c, verdict)) {
		case "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STALE":
			failed = true
		}
		if checkPending(c) {
			pendingCount++
		}
	}
	switch {
	case failed:
		return "failure", ""
	case pendingCount > 0:
		return "pending", strconv.Itoa(len(checks)-pendingCount) + "/" + strconv.Itoa(len(checks))
	}
	return "success", ""
}

func checkPending(c map[string]any) bool {
	switch strings.ToUpper(jqStr(c, "status")) {
	case "IN_PROGRESS", "QUEUED", "PENDING":
		return true
	}
	switch strings.ToUpper(jqStr(c, "state")) {
	case "EXPECTED", "PENDING":
		return true
	}
	return jqStr(c, "__typename") == "CheckRun" && jqStr(c, "conclusion") == ""
}
