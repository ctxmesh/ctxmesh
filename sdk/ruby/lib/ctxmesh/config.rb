# frozen_string_literal: true

module Ctxmesh
  # The resolved plane for one agent process.
  class Config
    DEFAULT_MEMORY_PORT = 2998
    DEFAULT_FEEDBACK_PORT = 2995
    DEFAULT_AMP_PORT = 2997
    DEFAULT_DELEGATE_PORT = 2994

    attr_reader :memory_port, :feedback_port, :amp_port, :delegate_port, :agent_name, :conversation_id

    def initialize(memory_port:, feedback_port:, amp_port:, delegate_port: DEFAULT_DELEGATE_PORT,
                   agent_name: "", conversation_id: "",
                   memory_wired: true, feedback_wired: true, long_term_enabled: true,
                   knowledge_enabled: true)
      @memory_port = memory_port
      @feedback_port = feedback_port
      @amp_port = amp_port
      @delegate_port = delegate_port
      @agent_name = agent_name
      @conversation_id = conversation_id
      @memory_wired = memory_wired
      @feedback_wired = feedback_wired
      @long_term_enabled = long_term_enabled
      @knowledge_enabled = knowledge_enabled
    end

    # False when MEMORY_PORT was absent: the platform did not grant memory to this agent.
    def memory_wired? = @memory_wired
    def feedback_wired? = @feedback_wired
    def long_term_enabled? = @long_term_enabled
    def knowledge_enabled? = @knowledge_enabled

    def memory_base = "http://127.0.0.1:#{@memory_port}"
    def feedback_base = "http://127.0.0.1:#{@feedback_port}"
    def amp_base = "http://127.0.0.1:#{@amp_port}"
    # Its OWN listener. /delegate and /handoff on the memory port are a 404.
    def delegate_base = "http://127.0.0.1:#{@delegate_port}"

    # Reads the launcher environment.
    def self.from_env(env = ENV)
      in_pod = %w[MEMORY_PORT FEEDBACK_PORT AGENT_NAME MODEL_GATEWAY_URL].any? { |k| env.key?(k) }
      raise NotInPodError, "ctxmesh: not running in a ctxmesh pod (no launcher environment)" unless in_pod

      mem, mem_set = port(env, "MEMORY_PORT", DEFAULT_MEMORY_PORT)
      fb, fb_set = port(env, "FEEDBACK_PORT", DEFAULT_FEEDBACK_PORT)
      # A2A_PORT is what the launcher publishes; AMP_PORT was invented.
      amp, = port(env, "A2A_PORT", DEFAULT_AMP_PORT)
      del, = port(env, "DELEGATE_PORT", DEFAULT_DELEGATE_PORT)

      new(
        memory_port: mem, feedback_port: fb, amp_port: amp, delegate_port: del,
        agent_name: env.fetch("AGENT_NAME", "").strip,
        conversation_id: env.fetch("CONVERSATION_ID", "").strip,
        memory_wired: mem_set, feedback_wired: fb_set,
        long_term_enabled: env.fetch("MEMORY_LONGTERM_ENABLED", "").strip == "true",
        knowledge_enabled: env.fetch("KNOWLEDGE_BASE_ENABLED", "").strip == "true"
      )
    end

    # Returns [value, was_explicitly_set]. The caller needs the difference: an unset port means
    # the capability is not wired, not that it is on the default.
    def self.port(env, name, default)
      raw = env[name]
      return [default, false] if raw.nil? || raw.strip.empty?

      n = Integer(raw.strip, exception: false)
      unless n && n.between?(1, 65_535)
        raise NotInPodError, "ctxmesh: #{name}=#{raw.inspect} is not a valid port"
      end

      [n, true]
    end
    private_class_method :port
  end
end
