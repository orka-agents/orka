---
slug: /cli-commands
---

# CLI Command Reference

This page is generated from `orka --help` output. Do not edit it by hand; run:

```bash
make docs-cli
```

For workflow-oriented examples and coverage notes, see [CLI Reference](./cli.md).

## `orka`

```text
Orka CLI — Kubernetes-native task execution platform

Usage:
  orka [command]

Available Commands:
  agent       Manage agents
  audit       Inspect audit and transaction traces
  auth        Inspect authentication
  completion  Generate the autocompletion script for the specified shell
  config      Manage CLI configuration
  help        Help about any command
  login       Authenticate with the Orka dashboard
  memory      Manage durable memory
  models      List compatible model IDs
  monitor     Manage repository monitors
  provider    Manage providers
  run         Chat with the Orka AI assistant
  secret      Inspect secret metadata
  security    Manage repository security scans
  session     Manage sessions
  skill       Manage skills
  status      Show system overview (health, tasks, agents)
  substrate   Inspect and manage substrate resources
  task        Manage tasks
  tool        Manage tools
  workspace   Inspect task workspace status

Flags:
  -h, --help                    help for orka
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
  -v, --version                 version for orka

Use "orka [command] --help" for more information about a command.
```

## `orka login`

```text
Generate a ServiceAccount token and open the Orka dashboard in your browser.

Usage:
  orka login [flags]

Flags:
  -h, --help                     help for login
      --no-open                  Print the login URL without opening a browser
      --redact-token             Redact the token in printed output while preserving browser login
      --service-account string   ServiceAccount name (default "default")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka run`

```text
Ollama-style chat interface.

  One-shot:    orka run "explain kubernetes pods"
  Interactive: orka run
  Piped:       echo "fix bugs" | orka run

Usage:
  orka run [prompt] [flags]

Flags:
      --agent string      Agent to use for the task
  -h, --help              help for run
      --model string      Model to use
      --provider string   Provider to use
      --session string    Resume a specific session
  -v, --verbose count     Verbosity level (-v, -vv)

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka config`

```text
Manage CLI configuration

Usage:
  orka config [command]

Available Commands:
  set-namespace Set the default Kubernetes namespace
  set-server    Set the default Orka server URL
  set-token     Set the default authentication token
  view          Show current configuration

Flags:
  -h, --help   help for config

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka config [command] --help" for more information about a command.
```

## `orka config set-server`

```text
Set the default Orka server URL

Usage:
  orka config set-server <url> [flags]

Flags:
  -h, --help   help for set-server

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka config set-token`

```text
Set the default authentication token

Usage:
  orka config set-token [token] [flags]

Flags:
  -f, --file string   Read token from file (use - for stdin)
  -h, --help          help for set-token

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka config set-namespace`

```text
Set the default Kubernetes namespace

Usage:
  orka config set-namespace <namespace> [flags]

Flags:
  -h, --help   help for set-namespace

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka config view`

```text
Show current configuration

Usage:
  orka config view [flags]

Flags:
  -h, --help   help for view

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka auth`

```text
Inspect authentication

Usage:
  orka auth [command]

Available Commands:
  validate    Validate current credentials
  whoami      Show sanitized authenticated identity

Flags:
  -h, --help   help for auth

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka auth [command] --help" for more information about a command.
```

## `orka auth validate`

```text
Validate current credentials

Usage:
  orka auth validate [flags]

Flags:
  -h, --help            help for validate
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka auth whoami`

```text
Show sanitized authenticated identity

Usage:
  orka auth whoami [flags]

Flags:
  -h, --help            help for whoami
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka models`

```text
List compatible model IDs

Usage:
  orka models [command]

Available Commands:
  list        List model IDs

Flags:
  -h, --help   help for models

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka models [command] --help" for more information about a command.
```

## `orka models list`

```text
List model IDs

Usage:
  orka models list [flags]

Flags:
      --compat string   Compatibility surface: openai or anthropic (default "openai")
  -h, --help            help for list
  -o, --output string   Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka status`

```text
Show system overview (health, tasks, agents)

Usage:
  orka status [flags]

Flags:
  -h, --help   help for status

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka audit`

```text
Inspect audit and transaction traces

Usage:
  orka audit [command]

Available Commands:
  trace       Show tasks correlated by kontxt transaction ID

Flags:
  -h, --help   help for audit

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka audit [command] --help" for more information about a command.
```

## `orka audit trace`

```text
Show tasks correlated by kontxt transaction ID

Usage:
  orka audit trace <transaction-id> [flags]

Flags:
  -h, --help        help for trace
      --limit int   Maximum number of matching tasks to show (default 100)

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task`

```text
Manage tasks

Usage:
  orka task [command]

Available Commands:
  artifacts   List artifacts for a task
  children    List child tasks
  create      Create a new task
  delete      Delete a task
  download    Download task artifacts
  get         Get task details
  list        List tasks
  logs        Get task logs
  plan        Get task autonomous plan state
  result      Get task result
  wait        Wait for a task to complete

Flags:
  -h, --help   help for task

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka task [command] --help" for more information about a command.
```

## `orka task create`

```text
Create a new task

Usage:
  orka task create <prompt> [flags]

Flags:
      --agent string          Agent reference name
      --arg stringArray       Command argument (repeat for multiple arguments)
      --command stringArray   Command entry to run (repeat for multiple entries)
      --env stringArray       Environment variable KEY=VALUE (repeatable)
  -f, --file string           Path to task YAML/JSON manifest
  -h, --help                  help for create
      --image string          Container image
      --model string          Model name for AI tasks
      --name string           Task name (default: generated)
      --priority int32        Task priority (0-1000)
      --provider string       Provider reference name (default "default")
      --schedule string       Cron schedule for recurring tasks
      --suspend               Suspend scheduled task runs
      --timeout string        Task timeout (e.g., "5m", "1h")
      --timezone string       IANA time zone for scheduled tasks
      --type string           Task type: ai, container, agent (default "ai")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task list`

```text
List tasks

Usage:
  orka task list [flags]

Aliases:
  list, ls

Flags:
      --continue string      Continue token for the next page
      --cursor string        Cursor token for the next page
  -h, --help                 help for list
      --limit int            Maximum number of results (default 20)
  -o, --output string        Output format: table, json, yaml (default "table")
      --status string        Filter by status (client-side scan; may page through many tasks)
      --transaction string   Filter by kontxt transaction ID (client-side scan)

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task get`

```text
Get task details

Usage:
  orka task get <name> [flags]

Flags:
  -h, --help               help for get
  -o, --output string      Output format: table, json, yaml (default "json")
      --show-transaction   Show only transaction metadata

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task logs`

```text
Get task logs

Usage:
  orka task logs <name> [flags]

Flags:
  -f, --follow   Stream logs in real time
  -h, --help     help for logs

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task result`

```text
Get task result

Usage:
  orka task result <name> [flags]

Flags:
  -h, --help            help for result
  -o, --output string   Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task plan`

```text
Get task autonomous plan state

Usage:
  orka task plan <name> [flags]

Flags:
  -h, --help            help for plan
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task children`

```text
List child tasks

Usage:
  orka task children <name> [flags]

Flags:
  -h, --help            help for children
  -o, --output string   Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task wait`

```text
Wait for a task to complete

Usage:
  orka task wait <name> [flags]

Flags:
  -h, --help             help for wait
      --timeout string   Maximum time to wait (e.g. 5m)

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task delete`

```text
Delete a task

Usage:
  orka task delete <name> [flags]

Aliases:
  delete, rm

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task artifacts`

```text
List artifacts for a task

Usage:
  orka task artifacts <task-name> [flags]

Flags:
  -h, --help   help for artifacts

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka task download`

```text
Download task artifacts

Usage:
  orka task download <task-name> [filename] [flags]

Flags:
  -h, --help            help for download
  -o, --output string   output file path

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka workspace`

```text
Inspect task workspace status

Usage:
  orka workspace [command]

Available Commands:
  status      Show safe workspace status fields

Flags:
  -h, --help   help for workspace

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka workspace [command] --help" for more information about a command.
```

## `orka workspace status`

```text
Show safe workspace status fields

Usage:
  orka workspace status <task> [flags]

Flags:
  -h, --help            help for status
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka provider`

```text
Manage providers

Usage:
  orka provider [command]

Available Commands:
  create      Create a provider resource from a manifest
  delete      Delete a provider resource
  get         Get a provider resource
  list        List provider resources
  update      Update a provider resource from a manifest

Flags:
  -h, --help   help for provider

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka provider [command] --help" for more information about a command.
```

## `orka provider list`

```text
List provider resources

Usage:
  orka provider list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka provider get`

```text
Get a provider resource

Usage:
  orka provider get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka provider create`

```text
Create a provider resource from a manifest

Usage:
  orka provider create -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka provider update`

```text
Update a provider resource from a manifest

Usage:
  orka provider update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka provider delete`

```text
Delete a provider resource

Usage:
  orka provider delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka agent`

```text
Manage agents

Usage:
  orka agent [command]

Available Commands:
  create      Create an agent from a manifest
  delete      Delete an agent
  get         Get agent details
  list        List agents
  update      Update an agent resource from a manifest

Flags:
  -h, --help   help for agent

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka agent [command] --help" for more information about a command.
```

## `orka agent list`

```text
List agents

Usage:
  orka agent list [flags]

Flags:
  -h, --help            help for list
  -o, --output string   Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka agent get`

```text
Get agent details

Usage:
  orka agent get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka agent create`

```text
Create an agent from a manifest

Usage:
  orka agent create -f <file> [flags]

Flags:
  -f, --file string   Path to agent YAML/JSON manifest
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka agent update`

```text
Update an agent resource from a manifest

Usage:
  orka agent update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka agent delete`

```text
Delete an agent

Usage:
  orka agent delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka tool`

```text
Manage tools

Usage:
  orka tool [command]

Available Commands:
  create      Create a tool resource from a manifest
  delete      Delete a tool resource
  get         Get a tool resource
  list        List tool resources
  update      Update a tool resource from a manifest

Flags:
  -h, --help   help for tool

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka tool [command] --help" for more information about a command.
```

## `orka tool list`

```text
List tool resources

Usage:
  orka tool list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka tool get`

```text
Get a tool resource

Usage:
  orka tool get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka tool create`

```text
Create a tool resource from a manifest

Usage:
  orka tool create -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka tool update`

```text
Update a tool resource from a manifest

Usage:
  orka tool update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka tool delete`

```text
Delete a tool resource

Usage:
  orka tool delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill`

```text
Manage skills

Usage:
  orka skill [command]

Available Commands:
  content     Print raw skill content
  create      Create a skill from a YAML manifest
  delete      Delete a skill
  get         Get skill details
  import      Create a skill from a local SKILL.md file
  init        Initialize a local SKILL.md template
  list        List skills
  update      Update a skill resource from a manifest
  validate    Validate a local skill manifest or SKILL.md file

Flags:
  -h, --help   help for skill

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka skill [command] --help" for more information about a command.
```

## `orka skill list`

```text
List skills

Usage:
  orka skill list [flags]

Flags:
  -h, --help            help for list
  -o, --output string   Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill get`

```text
Get skill details

Usage:
  orka skill get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill content`

```text
Print raw skill content

Usage:
  orka skill content <name> [flags]

Flags:
  -h, --help   help for content

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill create`

```text
Create a skill from a YAML manifest

Usage:
  orka skill create -f <file> [flags]

Flags:
  -f, --file string   Path to skill YAML manifest
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill init`

```text
Initialize a local SKILL.md template

Usage:
  orka skill init [dir] [flags]

Flags:
      --description string   Skill description for the template
      --force                Overwrite an existing SKILL.md
  -h, --help                 help for init
      --name string          Skill name for the template

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill validate`

```text
Validate a local skill manifest or SKILL.md file

Usage:
  orka skill validate [-f manifest.yaml] [SKILL.md] [flags]

Flags:
  -f, --file string   Path to skill YAML/JSON manifest
  -h, --help          help for validate

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill import`

```text
Create a skill from a local SKILL.md file

Usage:
  orka skill import <path/to/SKILL.md> [flags]

Flags:
  -h, --help          help for import
      --name string   Override skill name (default: derived from filename)

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill update`

```text
Update a skill resource from a manifest

Usage:
  orka skill update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka skill delete`

```text
Delete a skill

Usage:
  orka skill delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka secret`

```text
Inspect secret metadata

Usage:
  orka secret [command]

Available Commands:
  list        List secret resources

Flags:
  -h, --help   help for secret

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka secret [command] --help" for more information about a command.
```

## `orka secret list`

```text
List secret resources

Usage:
  orka secret list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka session`

```text
Manage sessions

Usage:
  orka session [command]

Available Commands:
  delete      Delete a session resource
  get         Get a session resource
  list        List session resources

Flags:
  -h, --help   help for session

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka session [command] --help" for more information about a command.
```

## `orka session list`

```text
List session resources

Usage:
  orka session list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka session get`

```text
Get a session resource

Usage:
  orka session get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka session delete`

```text
Delete a session resource

Usage:
  orka session delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory`

```text
Manage durable memory

Usage:
  orka memory [command]

Available Commands:
  create      Create a memory
  delete      Delete a memory
  disable     Disable a memory
  enable      Enable a memory
  get         Get a memory
  list        List memories
  proposal    Manage memory proposals
  update      Update a memory

Flags:
  -h, --help   help for memory

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka memory [command] --help" for more information about a command.
```

## `orka memory list`

```text
List memories

Usage:
  orka memory list [flags]

Flags:
      --agentName string     Filter by agentName
  -h, --help                 help for list
      --ids string           Filter by ids
      --include-deleted      Include deleted memories
      --include-disabled     Include disabled memories
      --limit int            Maximum number of results (default 100)
  -o, --output string        Output format: table, json, yaml (default "table")
      --parentTask string    Filter by parentTask
      --query string         Filter by query
      --sessionName string   Filter by sessionName
      --source string        Filter by source
      --tags string          Filter by tags
      --taskName string      Filter by taskName

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory get`

```text
Get a memory

Usage:
  orka memory get <id> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory create`

```text
Create a memory

Usage:
  orka memory create [flags]

Flags:
      --content string   Memory content
  -f, --file string      Path to memory JSON/YAML body
  -h, --help             help for create
      --source string    Memory source (default "cli")
      --tags string      Comma-separated tags

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory update`

```text
Update a memory

Usage:
  orka memory update <id> [flags]

Flags:
      --content string   Memory content
  -f, --file string      Path to memory JSON/YAML body
  -h, --help             help for update
      --source string    Memory source
      --tags string      Comma-separated tags

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory delete`

```text
Delete a memory

Usage:
  orka memory delete <id> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory enable`

```text
Enable a memory

Usage:
  orka memory enable <id> [flags]

Flags:
  -h, --help   help for enable

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory disable`

```text
Disable a memory

Usage:
  orka memory disable <id> [flags]

Flags:
  -h, --help   help for disable

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory proposal`

```text
Manage memory proposals

Usage:
  orka memory proposal [command]

Available Commands:
  apply       Apply an accepted memory proposal
  archive     Archive a memory proposal
  get         Get a memory proposal
  list        List memory proposals
  review      Review a memory proposal

Flags:
  -h, --help   help for proposal

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka memory proposal [command] --help" for more information about a command.
```

## `orka memory proposal list`

```text
List memory proposals

Usage:
  orka memory proposal list [flags]

Flags:
      --agent-name string   Filter by agent name
  -h, --help                help for list
      --limit int           Maximum number of results (default 100)
  -o, --output string       Output format: table, json, yaml (default "table")
      --query string        Search query
      --status string       Filter by proposal status
      --task-name string    Filter by task name
      --type string         Filter by proposal type

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory proposal get`

```text
Get a memory proposal

Usage:
  orka memory proposal get <id> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory proposal review`

```text
Review a memory proposal

Usage:
  orka memory proposal review <id> [flags]

Flags:
  -h, --help              help for review
      --note string       Review note
      --reviewer string   Reviewer name
      --status string     Review status (accepted or rejected)

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory proposal apply`

```text
Apply an accepted memory proposal

Usage:
  orka memory proposal apply <id> [flags]

Flags:
      --applied-by string   Reviewer applying the proposal
  -h, --help                help for apply

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka memory proposal archive`

```text
Archive a memory proposal

Usage:
  orka memory proposal archive <id> [flags]

Flags:
  -h, --help   help for archive

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security`

```text
Manage repository security scans

Usage:
  orka security [command]

Available Commands:
  dropped-findings Inspect dropped findings
  finding          Manage security findings
  repo             Manage security repository scan configs
  scan             Run and list security scan runs
  slice            Inspect security review slices
  threat-model     Manage security threat models

Flags:
  -h, --help   help for security

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security [command] --help" for more information about a command.
```

## `orka security repo`

```text
Manage security repository scan configs

Usage:
  orka security repo [command]

Available Commands:
  create      Create a repository scan resource from a manifest
  delete      Delete a repository scan resource
  get         Get a repository scan resource
  list        List repository scan resources
  update      Update a repository scan resource from a manifest

Flags:
  -h, --help   help for repo

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security repo [command] --help" for more information about a command.
```

## `orka security repo list`

```text
List repository scan resources

Usage:
  orka security repo list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security repo get`

```text
Get a repository scan resource

Usage:
  orka security repo get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security repo create`

```text
Create a repository scan resource from a manifest

Usage:
  orka security repo create -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security repo update`

```text
Update a repository scan resource from a manifest

Usage:
  orka security repo update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security repo delete`

```text
Delete a repository scan resource

Usage:
  orka security repo delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security scan`

```text
Run and list security scan runs

Usage:
  orka security scan [command]

Available Commands:
  list        List security scan runs
  run         Run a manual security scan

Flags:
  -h, --help   help for scan

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security scan [command] --help" for more information about a command.
```

## `orka security scan run`

```text
Run a manual security scan

Usage:
  orka security scan run <repo> [flags]

Flags:
  -h, --help   help for run

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security scan list`

```text
List security scan runs

Usage:
  orka security scan list <repo> [flags]

Flags:
      --continue string   Continue token
      --cursor string     Cursor token
  -h, --help              help for list
      --limit int         Maximum number of results (default 20)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security threat-model`

```text
Manage security threat models

Usage:
  orka security threat-model [command]

Available Commands:
  get         Get latest threat model
  update      Update threat model

Flags:
  -h, --help   help for threat-model

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security threat-model [command] --help" for more information about a command.
```

## `orka security threat-model get`

```text
Get latest threat model

Usage:
  orka security threat-model get <repo> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security threat-model update`

```text
Update threat model

Usage:
  orka security threat-model update <repo> [flags]

Flags:
      --content string   Threat model content
  -f, --file string      Path to threat model content
  -h, --help             help for update
      --source string    Threat model source (default "edited")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding`

```text
Manage security findings

Usage:
  orka security finding [command]

Available Commands:
  dismiss     dismiss a security finding
  get         Get a security finding
  list        List security findings
  patch       patch a security finding
  patches     List security patch proposals
  pr          Create a pull request for the latest patch proposal
  reopen      reopen a security finding
  validate    validate a security finding

Flags:
  -h, --help   help for finding

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security finding [command] --help" for more information about a command.
```

## `orka security finding list`

```text
List security findings

Usage:
  orka security finding list <repo> [flags]

Flags:
      --category string            Filter by category
      --continue string            Continue token
      --cursor string              Cursor token
  -h, --help                       help for list
      --limit int                  Maximum number of results (default 50)
  -o, --output string              Output format: table, json, yaml (default "table")
      --recommended                Only recommended findings
      --severity string            Filter by severity
      --slice-id string            Filter by slice ID
      --state string               Filter by state
      --validation-status string   Filter by validation status

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding get`

```text
Get a security finding

Usage:
  orka security finding get <id> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding dismiss`

```text
dismiss a security finding

Usage:
  orka security finding dismiss <id> [flags]

Flags:
  -h, --help   help for dismiss

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding reopen`

```text
reopen a security finding

Usage:
  orka security finding reopen <id> [flags]

Flags:
  -h, --help   help for reopen

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding validate`

```text
validate a security finding

Usage:
  orka security finding validate <id> [flags]

Flags:
  -h, --help   help for validate

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding patch`

```text
patch a security finding

Usage:
  orka security finding patch <id> [flags]

Flags:
  -h, --help            help for patch
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding patches`

```text
List security patch proposals

Usage:
  orka security finding patches <id> [flags]

Flags:
  -h, --help            help for patches
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security finding pr`

```text
Create a pull request for the latest patch proposal

Usage:
  orka security finding pr <id> [flags]

Flags:
  -h, --help            help for pr
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security slice`

```text
Inspect security review slices

Usage:
  orka security slice [command]

Available Commands:
  get         Get a security review slice
  list        List security review slices

Flags:
  -h, --help   help for slice

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security slice [command] --help" for more information about a command.
```

## `orka security slice list`

```text
List security review slices

Usage:
  orka security slice list <repo> [flags]

Flags:
      --continue string   Continue token
      --cursor string     Cursor token
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")
      --status string     Filter by status

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security slice get`

```text
Get a security review slice

Usage:
  orka security slice get <repo> <slice-id> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka security dropped-findings`

```text
Inspect dropped findings

Usage:
  orka security dropped-findings [command]

Available Commands:
  list        List dropped security findings

Flags:
  -h, --help   help for dropped-findings

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka security dropped-findings [command] --help" for more information about a command.
```

## `orka security dropped-findings list`

```text
List dropped security findings

Usage:
  orka security dropped-findings list <repo> [flags]

Flags:
      --continue string   Continue token
      --cursor string     Cursor token
  -h, --help              help for list
      --limit int         Maximum number of results (default 50)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor`

```text
Manage repository monitors

Usage:
  orka monitor [command]

Available Commands:
  create      Create a repository monitor resource from a manifest
  delete      Delete a repository monitor resource
  events      List repository monitor events
  get         Get a repository monitor resource
  items       List repository monitor items
  list        List repository monitor resources
  run         Trigger a manual repository monitor run
  runs        List repository monitor runs
  update      Update a repository monitor resource from a manifest

Flags:
  -h, --help   help for monitor

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka monitor [command] --help" for more information about a command.
```

## `orka monitor list`

```text
List repository monitor resources

Usage:
  orka monitor list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor get`

```text
Get a repository monitor resource

Usage:
  orka monitor get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor create`

```text
Create a repository monitor resource from a manifest

Usage:
  orka monitor create -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor update`

```text
Update a repository monitor resource from a manifest

Usage:
  orka monitor update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor delete`

```text
Delete a repository monitor resource

Usage:
  orka monitor delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor run`

```text
Trigger a manual repository monitor run

Usage:
  orka monitor run <name> [flags]

Flags:
  -h, --help                 help for run
      --target-kind string   Target kind (e.g. pull_request)
      --target-number int    Target number
      --target-sha string    Target commit SHA

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor runs`

```text
List repository monitor runs

Usage:
  orka monitor runs <name> [flags]

Flags:
      --continue string      Continue token
      --cursor string        Cursor token
  -h, --help                 help for runs
      --limit int            Maximum number of results (default 20)
  -o, --output string        Output format: table, json, yaml (default "table")
      --target-kind string   Filter by target kind
      --trigger string       Filter by trigger

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor items`

```text
List repository monitor items

Usage:
  orka monitor items <name> [flags]

Flags:
      --automerge-state string   Filter by automerge state
      --continue string          Continue token
      --cursor string            Cursor token
  -h, --help                     help for items
      --kind string              Filter by item kind
      --limit int                Maximum number of results (default 50)
  -o, --output string            Output format: table, json, yaml (default "table")
      --repair-state string      Filter by repair state
      --state string             Filter by state
      --verdict string           Filter by review verdict

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka monitor events`

```text
List repository monitor events

Usage:
  orka monitor events [name] [flags]

Flags:
      --continue string     Continue token
      --cursor string       Cursor token
      --event-type string   Filter by event type
  -h, --help                help for events
      --item-kind string    Filter by item kind
      --item-number int     Filter by item number
      --limit int           Maximum number of results (default 50)
      --name string         Repository monitor name
  -o, --output string       Output format: table, json, yaml (default "table")
      --run-id string       Filter by run ID

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka substrate`

```text
Inspect and manage substrate resources

Usage:
  orka substrate [command]

Available Commands:
  pool        Manage substrate actor pools

Flags:
  -h, --help   help for substrate

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka substrate [command] --help" for more information about a command.
```

## `orka substrate pool`

```text
Manage substrate actor pools

Usage:
  orka substrate pool [command]

Available Commands:
  create      Create a substrate pool resource from a manifest
  delete      Delete a substrate pool resource
  get         Get a substrate pool resource
  list        List substrate pool resources
  update      Update a substrate pool resource from a manifest

Flags:
  -h, --help   help for pool

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka substrate pool [command] --help" for more information about a command.
```

## `orka substrate pool list`

```text
List substrate pool resources

Usage:
  orka substrate pool list [flags]

Flags:
      --continue string   Continue/cursor token for the next page
      --cursor string     Cursor token for the next page
  -h, --help              help for list
      --limit int         Maximum number of results (default 100)
  -o, --output string     Output format: table, json, yaml (default "table")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka substrate pool get`

```text
Get a substrate pool resource

Usage:
  orka substrate pool get <name> [flags]

Flags:
  -h, --help            help for get
  -o, --output string   Output format: table, json, yaml (default "json")

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka substrate pool create`

```text
Create a substrate pool resource from a manifest

Usage:
  orka substrate pool create -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for create

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka substrate pool update`

```text
Update a substrate pool resource from a manifest

Usage:
  orka substrate pool update <name> -f <file> [flags]

Flags:
  -f, --file string   Path to YAML/JSON manifest (use - for stdin)
  -h, --help          help for update

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka substrate pool delete`

```text
Delete a substrate pool resource

Usage:
  orka substrate pool delete <name> [flags]

Flags:
  -h, --help   help for delete

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)
```

## `orka completion`

```text
Generate the autocompletion script for orka for the specified shell.
See each sub-command's help for details on how to use the generated script.

Usage:
  orka completion [command]

Available Commands:
  bash        Generate the autocompletion script for bash
  fish        Generate the autocompletion script for fish
  powershell  Generate the autocompletion script for powershell
  zsh         Generate the autocompletion script for zsh

Flags:
  -h, --help   help for completion

Global Flags:
      --kubeconfig string       Path to kubeconfig file
  -n, --namespace string        Kubernetes namespace (default "default")
  -s, --server string           Orka server URL (default "http://localhost:8080")
  -t, --token string            Bearer token for authentication
      --txn-token string        Kontxt transaction token to send via Txn-Token header
      --txn-token-file string   Path to file containing a Kontxt transaction token (use - for stdin)

Use "orka completion [command] --help" for more information about a command.
```

