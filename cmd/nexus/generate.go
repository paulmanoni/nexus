package main

import (
	"io"

	"github.com/spf13/cobra"
)

// newGenerateCmd builds the `nexus generate` parent that hosts
// production-deploy artifact generators. Each subcommand reads
// nexus.deploy.yaml and emits something an operator can run with
// without retyping the topology — the manifest stays the single
// source of truth for ports, peers, and deployment units.
//
// Today's children:
//
//	nexus generate dockerfile  — multi-stage Dockerfile per deployment
//	nexus generate bake        — docker-bake.hcl that builds every
//	                             deployment in one BuildKit run with
//	                             shared cache mounts
func newGenerateCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Emit production-deploy artifacts from nexus.deploy.yaml",
		Long: `Generate production-deploy artifacts from nexus.deploy.yaml.

The manifest is the single source of truth for the project's deployment
topology — ports, peers, owned modules. These generators turn that
declaration into something an operator can run with.

Each subcommand is documented under nexus help generate <subcommand>.`,
	}
	cmd.AddCommand(
		newGenerateDockerfileCmd(stdout, stderr),
		newGenerateBakeCmd(stdout, stderr),
	)
	return cmd
}
