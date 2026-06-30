package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var ticketsCmd = &cobra.Command{
	Use:   "tickets",
	Short: "Manage tickets and comments",
	RunE: func(cmd *cobra.Command, args []string) error {
		p := tea.NewProgram(standaloneWrap{newBoardModel()}, tea.WithAltScreen())
		_, err := p.Run()
		return err
	},
}

func init() {
	// ── armory tickets create ─────────────────────────────────────────────────

	var (
		ticketTitle     string
		ticketDesc      string
		ticketPriority  string
		ticketTimescale string
		ticketDueDate   string
		ticketAssignee  string
		ticketWorkflow  string
		ticketRun       string
		ticketForge     string
		ticketBoard     string
	)

	createTicketCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a ticket",
		Long: `Create a ticket with an optional priority and linked resources.

  armory tickets create --title "Fix login bug" --priority high
  armory tickets create --title "Deploy failed" --run <run-id> --priority critical`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ticketTitle == "" {
				return fmt.Errorf("--title is required")
			}
			payload := map[string]any{"title": ticketTitle}
			if ticketDesc != "" {
				payload["description"] = ticketDesc
			}
			if ticketPriority != "" {
				payload["priority"] = ticketPriority
			}
			if ticketTimescale != "" {
				payload["timescale"] = ticketTimescale
			}
			if ticketDueDate != "" {
				payload["due_date"] = ticketDueDate
			}
			if ticketAssignee != "" {
				payload["assignee_id"] = ticketAssignee
			}
			if ticketWorkflow != "" {
				payload["workflow_id"] = ticketWorkflow
			}
			if ticketRun != "" {
				payload["run_id"] = ticketRun
			}
			if ticketForge != "" {
				payload["forge_execution_id"] = ticketForge
			}
			if ticketBoard != "" {
				payload["board_id"] = ticketBoard
			}
			// Tag the ticket with the project the user is working in.
			if p := projectFilter(); p != "" {
				payload["project"] = p
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/tickets/tickets", body)
		},
	}
	createTicketCmd.Flags().StringVar(&ticketTitle, "title", "", "Ticket title (required)")
	createTicketCmd.Flags().StringVar(&ticketDesc, "description", "", "Ticket description")
	createTicketCmd.Flags().StringVar(&ticketPriority, "priority", "", "Priority value (e.g. low, medium, high, critical)")
	createTicketCmd.Flags().StringVar(&ticketTimescale, "timescale", "", "Timescale value (e.g. Q1 2026, sprint-3)")
	createTicketCmd.Flags().StringVar(&ticketDueDate, "due-date", "", "Due date in YYYY-MM-DD format")
	createTicketCmd.Flags().StringVar(&ticketAssignee, "assignee", "", "Assignee user ID")
	createTicketCmd.Flags().StringVar(&ticketWorkflow, "workflow", "", "Linked pipeline ID")
	createTicketCmd.Flags().StringVar(&ticketRun, "run", "", "Linked pipeline run ID")
	createTicketCmd.Flags().StringVar(&ticketForge, "forge-execution", "", "Linked forge execution ID")
	createTicketCmd.Flags().StringVar(&ticketBoard, "board", "", "Board ID to place the ticket on")

	// ── armory tickets list ───────────────────────────────────────────────────

	var (
		listStatus    string
		listPriority  string
		listTimescale string
		listAssignee  string
		listBoard     string
	)

	listTicketsCmd := &cobra.Command{
		Use:   "list",
		Short: "List tickets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if listStatus != "" {
				q.Set("status", listStatus)
			}
			if listPriority != "" {
				q.Set("priority", listPriority)
			}
			if listTimescale != "" {
				q.Set("timescale", listTimescale)
			}
			if listAssignee != "" {
				q.Set("assignee_id", listAssignee)
			}
			if listBoard != "" {
				q.Set("board_id", listBoard)
			}
			if p := projectFilter(); p != "" {
				q.Set("project", p)
			}
			path := "/tickets/tickets"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			return apiCall("GET", path, nil)
		},
	}
	listTicketsCmd.Flags().StringVar(&listStatus, "status", "", "Filter by status value")
	listTicketsCmd.Flags().StringVar(&listPriority, "priority", "", "Filter by priority value")
	listTicketsCmd.Flags().StringVar(&listTimescale, "timescale", "", "Filter by timescale value")
	listTicketsCmd.Flags().StringVar(&listAssignee, "assignee", "", "Filter by assignee user ID")
	listTicketsCmd.Flags().StringVar(&listBoard, "board", "", "Filter by board ID (\"none\" for unassigned)")

	// ── armory tickets get ────────────────────────────────────────────────────

	getTicketCmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a ticket with its comments",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/tickets/tickets/"+args[0], nil)
		},
	}

	// ── armory tickets update ─────────────────────────────────────────────────

	var (
		updateTitle     string
		updateDesc      string
		updateStatus    string
		updatePriority  string
		updateTimescale string
		updateDueDate   string
		updateAssignee  string
		updateWorkflow  string
		updateRun       string
		updateForge     string
	)

	updateTicketCmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a ticket",
		Long: `Update a ticket. Title is required; other fields keep their current value if omitted.

  armory tickets update <id> --title "Fix login bug" --status in_progress
  armory tickets update <id> --title "Deploy failed" --status resolved --priority low`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if updateTitle == "" {
				// Fetch current title to satisfy the API requirement.
				data, err := doRequest("GET", "/tickets/tickets/"+args[0], nil)
				if err != nil {
					return fmt.Errorf("fetching current ticket: %w", err)
				}
				var current struct {
					Title string `json:"title"`
				}
				if err := json.Unmarshal(data, &current); err != nil || current.Title == "" {
					return fmt.Errorf("could not determine ticket title; pass --title explicitly")
				}
				updateTitle = current.Title
			}
			payload := map[string]any{"title": updateTitle}
			if updateDesc != "" {
				payload["description"] = updateDesc
			}
			if updateStatus != "" {
				payload["status"] = updateStatus
			}
			if updatePriority != "" {
				payload["priority"] = updatePriority
			}
			if updateTimescale != "" {
				payload["timescale"] = updateTimescale
			}
			if updateDueDate != "" {
				payload["due_date"] = updateDueDate
			}
			if updateAssignee != "" {
				payload["assignee_id"] = updateAssignee
			}
			if updateWorkflow != "" {
				payload["workflow_id"] = updateWorkflow
			}
			if updateRun != "" {
				payload["run_id"] = updateRun
			}
			if updateForge != "" {
				payload["forge_execution_id"] = updateForge
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("PUT", "/tickets/tickets/"+args[0], body)
		},
	}
	updateTicketCmd.Flags().StringVar(&updateTitle, "title", "", "Ticket title (fetched automatically if omitted)")
	updateTicketCmd.Flags().StringVar(&updateDesc, "description", "", "Ticket description")
	updateTicketCmd.Flags().StringVar(&updateStatus, "status", "", "Status value")
	updateTicketCmd.Flags().StringVar(&updatePriority, "priority", "", "Priority value")
	updateTicketCmd.Flags().StringVar(&updateTimescale, "timescale", "", "Timescale value")
	updateTicketCmd.Flags().StringVar(&updateDueDate, "due-date", "", "Due date in YYYY-MM-DD format")
	updateTicketCmd.Flags().StringVar(&updateAssignee, "assignee", "", "Assignee user ID")
	updateTicketCmd.Flags().StringVar(&updateWorkflow, "workflow", "", "Linked pipeline ID")
	updateTicketCmd.Flags().StringVar(&updateRun, "run", "", "Linked pipeline run ID")
	updateTicketCmd.Flags().StringVar(&updateForge, "forge-execution", "", "Linked forge execution ID")

	// ── armory tickets delete ─────────────────────────────────────────────────

	deleteTicketCmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/tickets/tickets/"+args[0], nil)
		},
	}

	// ── armory tickets comment ────────────────────────────────────────────────

	commentCmd := &cobra.Command{
		Use:   "comment",
		Short: "Manage ticket comments",
	}

	var commentBody string

	addCommentCmd := &cobra.Command{
		Use:   "add <ticket-id>",
		Short: "Add a comment to a ticket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if commentBody == "" {
				return fmt.Errorf("--body is required")
			}
			body, err := json.Marshal(map[string]string{"body": commentBody})
			if err != nil {
				return err
			}
			return apiCall("POST", "/tickets/tickets/"+args[0]+"/comments", body)
		},
	}
	addCommentCmd.Flags().StringVar(&commentBody, "body", "", "Comment text (required)")

	deleteCommentCmd := &cobra.Command{
		Use:   "delete <ticket-id> <comment-id>",
		Short: "Delete a comment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/tickets/tickets/"+args[0]+"/comments/"+args[1], nil)
		},
	}

	commentCmd.AddCommand(addCommentCmd, deleteCommentCmd)

	// ── armory tickets boards ─────────────────────────────────────────────────

	boardsCmd := &cobra.Command{
		Use:   "boards",
		Short: "Manage ticket boards",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/tickets/boards", nil)
		},
	}

	var (
		boardName  string
		boardDesc  string
		boardColor string
	)

	createBoardCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a board",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if boardName == "" {
				return fmt.Errorf("--name is required")
			}
			payload := map[string]any{"name": boardName}
			if boardDesc != "" {
				payload["description"] = boardDesc
			}
			if boardColor != "" {
				payload["color"] = boardColor
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			return apiCall("POST", "/tickets/boards", body)
		},
	}
	createBoardCmd.Flags().StringVar(&boardName, "name", "", "Board name (required)")
	createBoardCmd.Flags().StringVar(&boardDesc, "description", "", "Board description")
	createBoardCmd.Flags().StringVar(&boardColor, "color", "", "Board color (hex, e.g. #aabbcc)")

	listBoardsCmd := &cobra.Command{
		Use:   "list",
		Short: "List boards",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("GET", "/tickets/boards", nil)
		},
	}

	deleteBoardCmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a board (its tickets are kept and become unassigned)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return apiCall("DELETE", "/tickets/boards/"+args[0], nil)
		},
	}

	boardsCmd.AddCommand(createBoardCmd, listBoardsCmd, deleteBoardCmd)

	ticketsCmd.AddCommand(
		createTicketCmd,
		listTicketsCmd,
		getTicketCmd,
		updateTicketCmd,
		deleteTicketCmd,
		commentCmd,
		boardsCmd,
	)
	RegisterModule(Module{
		Name:    "tickets",
		Service: "tickets",
		Order:   20,
		Command: ticketsCmd,
		Screens: []HubScreen{{
			Title: "Tickets Board",
			Desc:  "Interactive kanban board",
			New:   func() tea.Model { return newBoardModel() },
		}},
	})
}
