package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// pairedNameField maps an ID field to the companion name field that
// typically appears alongside it in the same API response.
var pairedNameField = map[string]string{
	"org_id":  "org_name",
	"team_id": "team_name",
	"user_id": "username",
}

// reverseNamePair maps a name field back to its companion ID field.
var reverseNamePair = map[string]string{
	"org_name":  "org_id",
	"team_name": "team_id",
	"username":  "user_id",
}

type resolverDef struct{ path, field string }

// foreignKeyResolvers defines how to look up a UUID field's human-readable name
// via the API when no companion name field is present in the same response.
// Also covers org_id/team_id/user_id as fallback when no companion name exists.
var foreignKeyResolvers = map[string]resolverDef{
	"org_id":       {"/orgs/%s", "org_name"},
	"team_id":      {"/teams/%s", "team_name"},
	"user_id":      {"/users/%s", "username"},
	"owner_id":     {"/users/%s", "username"},
	"created_by":   {"/users/%s", "username"},
	"triggered_by": {"/users/%s", "username"},
	"inviter_id":   {"/users/%s", "username"},
	"assignee_id":  {"/users/%s", "username"},
}

// nameCache is reset at the start of each printResponse call to deduplicate
// API lookups within a single render without caching across calls.
var nameCache map[string]string

// resolvedNameForID returns a human-readable name for a UUID field value.
// It checks the companion name field in obj first; if absent, makes an API call.
func resolvedNameForID(field, uuid string, obj map[string]any) string {
	if uuid == "" {
		return ""
	}
	if nameField, ok := pairedNameField[field]; ok {
		if name, ok := obj[nameField].(string); ok && name != "" {
			return name
		}
	}
	r, hasResolver := foreignKeyResolvers[field]
	if !hasResolver {
		return ""
	}
	cacheKey := field + ":" + uuid
	if name, cached := nameCache[cacheKey]; cached {
		return name
	}
	name := ""
	if data, err := doRequest("GET", fmt.Sprintf(r.path, uuid), nil); err == nil {
		var resp map[string]any
		if json.Unmarshal(data, &resp) == nil {
			if n, ok := resp[r.field].(string); ok {
				name = n
			}
		}
	}
	nameCache[cacheKey] = name
	return name
}

// printResponse formats an API response as human-readable output.
// JSON arrays are printed as tables; single objects as key-value records.
func printResponse(data []byte) {
	if len(data) == 0 || strings.TrimSpace(string(data)) == "null" {
		return
	}
	nameCache = map[string]string{}

	var arr []map[string]any
	if json.Unmarshal(data, &arr) == nil {
		if len(arr) == 0 {
			fmt.Println("No results.")
			return
		}
		printTable(arr)
		return
	}

	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		printRecord(obj)
		return
	}

	fmt.Println(string(data))
}

// printTable renders a slice of objects as an aligned, column-based table.
func printTable(rows []map[string]any) {
	cols := tableColumns(rows[0])
	if len(cols) == 0 {
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = strings.ToUpper(fieldLabel(c))
	}
	fmt.Fprintln(w, strings.Join(headers, "\t"))

	for _, row := range rows {
		vals := make([]string, len(cols))
		for i, c := range cols {
			vals[i] = tableCellValue(c, row)
		}
		fmt.Fprintln(w, strings.Join(vals, "\t"))
	}
	w.Flush()
}

// tableCellValue returns the display value for a single table cell, resolving
// UUID fields to names and appending the UUID in verbose mode.
func tableCellValue(col string, row map[string]any) string {
	v := row[col]

	// Name field with paired companion ID: verbose appends "(uuid)".
	if idField, ok := reverseNamePair[col]; ok {
		if idVal, ok := row[idField].(string); ok && idVal != "" && flagVerbose {
			return truncate(formatValue(v)+" ("+idVal+")", 52)
		}
		return truncate(formatValue(v), 40)
	}

	// Foreign key or user-reference field: resolve to name.
	if uuid, ok := v.(string); ok && !primaryIDs[col] {
		if strings.HasSuffix(col, "_id") || foreignKeyResolvers[col].path != "" {
			if name := resolvedNameForID(col, uuid, row); name != "" {
				if flagVerbose {
					return truncate(name+" ("+uuid+")", 52)
				}
				return truncate(name, 40)
			}
		}
	}

	return truncate(formatValue(v), 40)
}

// printRecord renders a single object as aligned key: value pairs.
// IDs are translated to names where possible. With -v, both name and UUID
// are shown, plus timestamps and boolean metadata.
func printRecord(obj map[string]any) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	for _, k := range sortedKeys(obj) {
		if skipInRecord[k] {
			continue
		}

		val := obj[k]

		// Name field with a companion ID in the same response.
		// Default: "Name". Verbose: "Name (uuid)".
		// The companion ID field is suppressed so it doesn't appear separately.
		if idField, ok := reverseNamePair[k]; ok {
			if idVal, ok := obj[idField].(string); ok {
				display := formatValue(val)
				if flagVerbose && idVal != "" {
					display += " (" + idVal + ")"
				}
				fmt.Fprintf(w, "%s\t%s\n", fieldLabel(k), display)
				continue
			}
		}

		// All *_id fields and user-reference fields (created_by, triggered_by, …).
		uuid, isStr := val.(string)
		if isStr && (strings.HasSuffix(k, "_id") || foreignKeyResolvers[k].path != "") {
			// Suppress if a companion name field in this response already handles it.
			if nameField, ok := pairedNameField[k]; ok {
				if _, hasName := obj[nameField]; hasName {
					continue
				}
			}
			name := resolvedNameForID(k, uuid, obj)
			switch {
			case name != "" && flagVerbose:
				fmt.Fprintf(w, "%s\t%s\n", fieldLabel(k), name+" ("+uuid+")")
			case name != "":
				fmt.Fprintf(w, "%s\t%s\n", fieldLabel(k), name)
			case flagVerbose && uuid != "":
				fmt.Fprintf(w, "%s\t%s\n", fieldLabel(k), uuid)
			}
			continue
		}

		// Timestamps and boolean metadata: verbose only.
		if !flagVerbose && isNoisyField(k) {
			continue
		}

		fmt.Fprintf(w, "%s\t%s\n", fieldLabel(k), formatValue(val))
	}

	w.Flush()
}

// tableColumns picks the columns to show in list output.
// Default: name/label fields and key status/type fields.
// Verbose: also includes primary IDs (where no companion name exists), created_at, active.
// Resolvable foreign key fields (created_by, etc.) are always included.
func tableColumns(row map[string]any) []string {
	var primary, names, extras, dates []string

	for k := range row {
		if skipInTable[k] {
			continue
		}
		switch {
		case strings.HasSuffix(k, "_at"):
			if flagVerbose && k == "created_at" {
				dates = append(dates, k)
			}

		case primaryIDs[k]:
			// Show as a standalone UUID column only in verbose and only when
			// no companion name column is present (which already carries the UUID).
			if flagVerbose {
				if nameField, ok := pairedNameField[k]; ok {
					if _, hasName := row[nameField]; hasName {
						continue // companion name column handles display
					}
				}
				primary = append(primary, k)
			}

		case strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_ids"):
			// Non-primary foreign key: include only when resolvable (shown as name).
			// Skip if the companion name field is already present (avoids duplication).
			if r := foreignKeyResolvers[k]; r.path != "" {
				if nameField, ok := pairedNameField[k]; ok {
					if _, hasName := row[nameField]; hasName {
						continue
					}
				}
				extras = append(extras, k)
			}

		case k == "active" || k == "enabled":
			if flagVerbose {
				extras = append(extras, k)
			}

		case k == "name" || strings.HasSuffix(k, "_name") ||
			k == "title" || k == "username" ||
			k == "email" || k == "invitee_email":
			names = append(names, k)

		case foreignKeyResolvers[k].path != "":
			// Non-_id user-reference fields (created_by, triggered_by, etc.).
			extras = append(extras, k)

		case k == "status" || k == "service" || k == "event_type" ||
			k == "priority" || k == "repo" || k == "runner_class" || k == "image":
			extras = append(extras, k)
		}
	}

	sort.Strings(primary)
	sort.Strings(names)
	sort.Strings(extras)
	sort.Strings(dates)

	var cols []string
	if len(primary) > 0 {
		cols = append(cols, primary[0])
	}
	cols = append(cols, names...)
	cols = append(cols, extras...)
	cols = append(cols, dates...)
	return cols
}

// isNoisyField returns true for timestamps and boolean metadata flags hidden
// in default output. ID fields are handled separately (resolved to names).
func isNoisyField(k string) bool {
	if strings.HasSuffix(k, "_at") {
		return true
	}
	switch k {
	case "active", "enabled":
		return true
	}
	return false
}

// formatValue converts a JSON-decoded value to a display string.
func formatValue(v any) string {
	if v == nil {
		return "-"
	}
	switch val := v.(type) {
	case bool:
		if val {
			return "yes"
		}
		return "no"
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%.2f", val)
	case string:
		if val == "" {
			return "-"
		}
		if t, err := time.Parse(time.RFC3339, val); err == nil {
			return t.Format("2006-01-02 15:04")
		}
		if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
			return t.Format("2006-01-02 15:04")
		}
		return val
	case []any:
		if len(val) == 0 {
			return "-"
		}
		strs := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return fmt.Sprintf("%d items", len(val))
			}
			strs = append(strs, s)
		}
		return strings.Join(strs, ", ")
	case map[string]any:
		if len(val) == 0 {
			return "-"
		}
		return fmt.Sprintf("{%d fields}", len(val))
	default:
		return fmt.Sprintf("%v", v)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// sortedKeys returns map keys ordered for human-readable record display.
func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, oj := keyOrder(keys[i]), keyOrder(keys[j])
		if oi != oj {
			return oi < oj
		}
		return keys[i] < keys[j]
	})
	return keys
}

func keyOrder(k string) int {
	if primaryIDs[k] {
		return 0
	}
	if k == "name" || strings.HasSuffix(k, "_name") || k == "title" ||
		k == "username" || k == "email" || k == "invitee_email" {
		return 1
	}
	if k == "status" || k == "service" || k == "event_type" ||
		k == "resource_type" || k == "priority" {
		return 2
	}
	if strings.HasSuffix(k, "_at") {
		return 9
	}
	if k == "active" || k == "enabled" {
		return 8
	}
	if strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_ids") {
		return 7
	}
	return 3
}

// fieldLabel returns a human-readable label for a JSON field name.
func fieldLabel(k string) string {
	if label, ok := fieldLabels[k]; ok {
		return label
	}
	parts := strings.Split(k, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// primaryIDs is the set of fields that act as a resource's own primary key.
var primaryIDs = map[string]bool{
	"org_id":         true,
	"team_id":        true,
	"user_id":        true,
	"role_id":        true,
	"permissions_id": true,
	"session_id":     true,
	"invite_id":      true,
	"rule_id":        true,
	"workflow_id":    true,
	"execution_id":   true,
	"run_id":         true,
	"ticket_id":      true,
	"step_id":        true,
	"event_id":       true,
	"trigger_id":     true,
	"comment_id":     true,
	"step_run_id":    true,
}

// skipInTable are fields omitted from table/list output (too verbose or redundant).
var skipInTable = map[string]bool{
	"stdout": true, "stderr": true, "payload": true, "description": true,
	"step_runs": true, "comments": true, "with": true, "env": true,
	"command": true, "input_mapping": true, "body_transforms": true,
	"inputs": true, "outputs": true, "updated_at": true,
}

// skipInRecord are fields omitted even from single-record display.
var skipInRecord = map[string]bool{
	"stdout": true, "stderr": true, "payload": true,
	"body_transforms": true,
}

var fieldLabels = map[string]string{
	"org_id":          "Org",
	"org_name":        "Name",
	"team_id":         "Team",
	"team_name":       "Name",
	"user_id":         "User",
	"role_id":         "Role ID",
	"permissions_id":  "Permission ID",
	"session_id":      "Session ID",
	"invite_id":       "Invite ID",
	"rule_id":         "Rule ID",
	"workflow_id":     "Workflow",
	"execution_id":    "Execution ID",
	"run_id":          "Run ID",
	"ticket_id":       "Ticket ID",
	"step_id":         "Step ID",
	"event_id":        "Event ID",
	"trigger_id":      "Trigger ID",
	"comment_id":      "Comment ID",
	"step_run_id":     "Step Run ID",
	"owner_id":        "Owner",
	"created_by":      "Created By",
	"triggered_by":    "Triggered By",
	"inviter_id":      "Invited By",
	"invitee_email":   "Invitee",
	"assignee_id":     "Assignee",
	"permissions_ids": "Permissions",
	"runner_class":    "Runner",
	"exit_code":       "Exit Code",
	"event_type":      "Event",
	"resource_type":   "Resource Type",
	"resource_id":     "Resource",
	"ref_filter":      "Ref Filter",
	"memory_mb":       "Memory (MB)",
	"cpu_millicores":  "CPU (mCores)",
	"pids_limit":      "PID Limit",
	"tmpfs_mb":        "Tmpfs (MB)",
	"rules_matched":   "Rules Matched",
	"step_index":      "Step #",
	"step_name":       "Step",
	"created_at":      "Created",
	"updated_at":      "Updated",
	"started_at":      "Started",
	"ended_at":        "Ended",
	"expires_at":      "Expires",
}
