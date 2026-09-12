# frozen_string_literal: true

require_relative "lib/ctxmesh/version"

Gem::Specification.new do |spec|
  spec.name = "ctxmesh"
  spec.version = Ctxmesh::VERSION
  spec.authors = ["ctxmesh"]

  spec.summary = "Ruby SDK for ctxmesh — the Kubernetes-native control plane for AI agents"
  # RubyGems renders no README — this description is the whole page body, so it has to say what
  # ctxmesh is and where the project lives, not just what the gem does.
  spec.description = "Ruby SDK for ctxmesh, the Kubernetes-native control plane for AI agents. " \
                     "Typed clients for conversation memory, long-term memory, knowledge bases, " \
                     "skills, feedback, delegation and agent-to-agent calls. Reads its endpoints " \
                     "from the environment the platform injects, so your code never holds " \
                     "credentials. Source, docs and issues: https://github.com/ctxmesh/ctxmesh"
  spec.homepage = "https://ctxmesh.github.io"
  spec.license = "Apache-2.0"
  spec.required_ruby_version = ">= 3.1"

  spec.metadata = {
    "homepage_uri" => "https://ctxmesh.github.io",
    "source_code_uri" => "https://github.com/ctxmesh/ctxmesh",
    "bug_tracker_uri" => "https://github.com/ctxmesh/ctxmesh/issues",
    "documentation_uri" => "https://ctxmesh.github.io/sdk/",
    "changelog_uri" => "https://github.com/ctxmesh/ctxmesh/blob/main/CHANGELOG.md",
    # Requires MFA to publish: this gem's name is a public identity, and a stolen API key
    # should not be enough to push a malicious version of it.
    "rubygems_mfa_required" => "true"
  }

  spec.files = Dir["lib/**/*.rb"] + ["README.md"]
  spec.require_paths = ["lib"]

  # No runtime dependencies. The plane is JSON over localhost HTTP, which net/http and the
  # stdlib json cover — and an SDK that pulls in a gem becomes a version conflict in every
  # application that already has it.
end
