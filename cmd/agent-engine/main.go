/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// agent-engine is the operator CLI for agent-brain managed agents.
// Commands:
//
//	expand <file>   — convert a simplified agent.yaml to an AgentDeployment CRD manifest
//	dev             — run the agent locally (launcher + full contract + mock gateway)
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "agent-engine",
		Short: "agent-engine — operator CLI for agent-brain managed agents",
		// No default run; subcommand required.
	}
	root.AddCommand(newExpandCmd())
	root.AddCommand(newDevCmd())
	root.AddCommand(newEvalCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
