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

// Package jsname spells the names the JetStream async backend is addressed by: the stream, the subject a
// hop is published on, and the durable consumer that holds one agent's queue. It imports no broker client,
// so the controller can name an agent's consumer in a KEDA ScaledObject without linking one, and the BFF
// that creates the consumer and the controller that points KEDA at it cannot disagree on a spelling.
package jsname

import "strings"

const (
	// Stream is the single stream carrying every registry's async AMP traffic.
	Stream = "CTXMESH_A2A"
	// SubjectPrefix namespaces the platform's subjects inside a NATS server an operator may share with
	// other workloads. The stream captures SubjectPrefix + ".>".
	SubjectPrefix = "ctxmesh.a2a"

	// agentDurablePrefix starts every per-agent durable consumer's name. The separators after it are
	// underscores, which a namespace or an agent name (both DNS labels) cannot contain, so two different
	// agents can never share a durable.
	agentDurablePrefix = "agent_"
)

// RegistrySubject is the subject a hop was published on before hops were addressed to their receiver:
// ctxmesh.a2a.<registryId>. It carries only the sender's registry.
func RegistrySubject(registryID string) string {
	return SubjectPrefix + "." + Token(registryID)
}

// RegistrySubjects matches every subject in the RegistrySubject form, and none in the AgentSubject form.
const RegistrySubjects = SubjectPrefix + ".*"

// AgentSubject is the subject a hop to one agent is published on: ctxmesh.a2a.<registryId>.<namespace>.<agent>,
// where the registry is the sender's and the namespace and agent are the receiver's. A registry id is unique
// only within its namespace, so the namespace is part of the address.
func AgentSubject(registryID, namespace, agent string) string {
	return RegistrySubject(registryID) + "." + Token(namespace) + "." + Token(agent)
}

// AgentFilter matches every hop addressed to one agent, whatever registry it was published into. The
// dispatcher refuses a hop whose registry is not the receiver's, so a crossing is still consumed, logged and
// acknowledged rather than left waiting unseen.
func AgentFilter(namespace, agent string) string {
	return SubjectPrefix + ".*." + Token(namespace) + "." + Token(agent)
}

// AgentDurable is the durable consumer holding one agent's queue. Its pending count is the number of hops
// waiting for that agent, which is what a queue-depth AgentScalingPolicy scales on.
func AgentDurable(namespace, agent string) string {
	return agentDurablePrefix + Token(namespace) + "_" + Token(agent)
}

// Token keeps a value inside one subject token. Registry ids, namespaces and agent names are DNS labels by
// validation, so this is defence in depth: a '.', '*' or '>' smuggled into a token would change which
// messages a filter matches.
func Token(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}
