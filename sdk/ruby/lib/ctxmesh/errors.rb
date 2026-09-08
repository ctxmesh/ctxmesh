# frozen_string_literal: true

module Ctxmesh
  # Base for everything this SDK raises.
  class Error < StandardError; end

  # The launcher environment is absent: this process is not running as a ctxmesh agent.
  # Raised rather than guessing ports, so running an agent on a laptop says so plainly instead
  # of failing later with a connection refused to localhost:2998.
  class NotInPodError < Error; end

  # The platform did not grant this capability to this agent. A configuration answer, not a
  # failure: the port is absent because nothing is listening.
  class NotWiredError < Error; end

  # A non-2xx response from the plane.
  class ApiError < Error
    attr_reader :status, :path, :body

    def initialize(status, path, body)
      @status = status
      @path = path
      @body = body
      super("ctxmesh: #{path} returned #{status}: #{body}")
    end
  end

  # A 403 — the plane understood the call and refused it. Guardrails, budgets and the delegate
  # fence surface here, and the reason survives: a refusal that says only "403" turns a policy
  # decision into a bare status code.
  class DeniedError < ApiError
    def initialize(path, body)
      super(403, path, body)
    end
  end
end
