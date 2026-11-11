/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
)

const defaultGuideAgent = "orka-guide"

// askPollInterval is how often orka ask checks the Task, and askNotFoundGrace
// how long a new Task may still read as not found: the API serves Task reads
// from its informer cache, which can lag the create. Tests shorten both.
var (
	askPollInterval  = 2 * time.Second
	askNotFoundGrace = 30 * time.Second
)

// askAnswer is the structured output of orka ask.
type askAnswer struct {
	Task   string `json:"task" yaml:"task"`
	Agent  string `json:"agent" yaml:"agent"`
	Phase  string `json:"phase,omitempty" yaml:"phase,omitempty"`
	Answer string `json:"answer,omitempty" yaml:"answer,omitempty"`
}

func newAskCmd() *cobra.Command {
	var agent string
	var timeout time.Duration
	var noWait bool

	cmd := &cobra.Command{
		Use:   "ask <question>",
		Short: "Ask the Orka guide a question about Orka",
		Long: `Ask the installation's Orka guide a question. The guide answers from the
docs of the installed Orka version and changes nothing: when something needs
changing, it gives you the commands to run.

  orka ask -n orka-system "how do I mount a toolbox into an Agent?"
  echo "which runtimes can mount toolboxes?" | orka ask -n orka-system -

The guide is the orka-guide Agent, which the controller creates when the Helm
chart sets guide.enabled=true. Each question runs as an agent Task in the
namespace, so asking needs permission to create Tasks there.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			question, err := askQuestion(cmd, args)
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			agent = strings.TrimSpace(agent)
			if agent == "" {
				return fmt.Errorf("--agent must name an Agent")
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be positive")
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c := newClientFromCmd(cmd)

			if _, err := c.GetAgent(ctx, agent, client.GetOptions{Namespace: c.Namespace}); err != nil &&
				strings.Contains(err.Error(), "HTTP 404") {
				return fmt.Errorf("no Agent %q in namespace %q: the Orka guide exists when the Helm chart sets guide.enabled=true. "+
					"Pass -n with the installation's namespace, or --agent to ask another Agent", agent, c.Namespace)
			}

			name, err := createAskTask(ctx, c, agent, question)
			if err != nil {
				return err
			}
			answer := askAnswer{Task: name, Agent: agent}
			if noWait {
				if format == outputTable {
					fmt.Fprintln(cmd.OutOrStdout(), name) //nolint:errcheck
					return nil
				}
				return printStructuredTo(cmd.OutOrStdout(), format, answer)
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "Asking %s (task %s)...\n", agent, name) //nolint:errcheck
			waitCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			detail, err := waitForAskTask(waitCtx, c, name)
			if err != nil {
				if waitCtx.Err() != nil {
					return fmt.Errorf("no answer yet; the question is still running as task %s. "+
						"Get the answer later with: orka task result %s -n %s", name, name, c.Namespace)
				}
				return err
			}
			answer.Phase = client.StringField(detail, "status", "phase")
			if !strings.EqualFold(answer.Phase, "Succeeded") {
				return askTaskFailure(name, c.Namespace, detail)
			}
			result, err := c.GetTaskResult(waitCtx, name, client.GetOptions{Namespace: c.Namespace})
			if err != nil {
				if waitCtx.Err() != nil {
					return fmt.Errorf("task %s finished, but reading its answer outlasted --timeout. "+
						"Get the answer with: orka task result %s -n %s", name, name, c.Namespace)
				}
				return fmt.Errorf("read the answer from task %s: %w", name, err)
			}
			answer.Answer = result.Result
			if strings.TrimSpace(answer.Answer) == "" {
				return fmt.Errorf("task %s succeeded without an answer. See orka task logs %s -n %s",
					name, name, c.Namespace)
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, answer)
			}
			printAskAnswer(cmd.OutOrStdout(), answer.Answer)
			return nil
		},
	}

	cmd.Flags().StringVar(&agent, "agent", defaultGuideAgent, "Agent that answers")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "How long to wait for the answer")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Create the Task, print its name and return without waiting")
	addOutputFlag(cmd, outputTable)
	return cmd
}

// askQuestion joins the arguments, or reads standard input for "-" or when
// input is piped and no arguments are given.
func askQuestion(cmd *cobra.Command, args []string) (string, error) {
	readStdin := len(args) == 1 && args[0] == "-"
	if len(args) == 0 {
		if stat, err := os.Stdin.Stat(); err == nil && stat.Mode()&os.ModeCharDevice == 0 {
			readStdin = true
		}
	}
	question := strings.Join(args, " ")
	if readStdin {
		// A token read from stdin would otherwise become the question and
		// reach the model.
		if tokenFile, _ := cmd.Flags().GetString("txn-token-file"); tokenFile == "-" {
			return "", fmt.Errorf("the question and --txn-token-file - can't both come from stdin: " +
				"pass the question as an argument or the token in a file")
		}
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read the question from stdin: %w", err)
		}
		question = string(data)
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("a question is required: orka ask \"<question>\"")
	}
	return question, nil
}

func createAskTask(ctx context.Context, c *client.Client, agent, question string) (string, error) {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	request := client.CreateTaskRequest{
		Name:      "ask-" + hex.EncodeToString(suffix),
		Namespace: c.Namespace,
		Type:      cliTaskTypeAgent,
		Prompt:    question,
		AgentRef: &struct {
			Name string `json:"name"`
		}{Name: agent},
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshal task request: %w", err)
	}
	created, err := c.CreateTaskRaw(ctx, body)
	if err != nil {
		// Name the Task: when the request's outcome is unknown, it may exist.
		return "", fmt.Errorf("create the question task %s: %w", request.Name, err)
	}
	if name := client.StringField(*created, "metadata", "name"); name != "" {
		return name, nil
	}
	return request.Name, nil
}

// waitForAskTask polls the Task until it reaches a terminal phase.
func waitForAskTask(ctx context.Context, c *client.Client, name string) (client.TaskDetail, error) {
	ticker := time.NewTicker(askPollInterval)
	defer ticker.Stop()
	notFoundUntil := time.Now().Add(askNotFoundGrace)
	seen := false
	for {
		detail, err := c.GetTask(ctx, name, client.GetOptions{Namespace: c.Namespace})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		switch {
		case err != nil && !seen && strings.Contains(err.Error(), "HTTP 404") && time.Now().Before(notFoundUntil):
			// Not visible to the API yet.
		case err != nil:
			return nil, fmt.Errorf("read task %s: %w", name, err)
		default:
			seen = true
			if taskPhaseIsTerminal(client.StringField(*detail, "status", "phase")) {
				return *detail, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func askTaskFailure(name, namespace string, detail client.TaskDetail) error {
	phase := client.StringField(detail, "status", "phase")
	message := fmt.Sprintf("task %s finished with phase %s", name, phase)
	if reason := taskFailureReason(nestedMap(detail, "status"), detail); reason != "" && reason != phase {
		message += ": " + reason
	}
	return errors.New(message + fmt.Sprintf(". See orka task get %s -n %s and orka task logs %s -n %s", name, namespace, name, namespace))
}

// printAskAnswer prints the answer as written, rendered as Markdown on a
// terminal.
func printAskAnswer(out io.Writer, text string) {
	if file, ok := out.(*os.File); ok && isTTYCheck(file) {
		if rendered := renderMarkdown(text); rendered != "" {
			fmt.Fprint(out, rendered) //nolint:errcheck
			return
		}
	}
	fmt.Fprint(out, text) //nolint:errcheck
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(out) //nolint:errcheck
	}
}
