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

package controller

import (
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// envSkillRefs lists the agent's PINNED skills, comma-separated, as
	// "<name>@sha256:…". Always digests: the launcher must never see an alias, because an
	// alias is a name whose meaning can change while the pod keeps running.
	//
	// Static env, no valueFrom — the Knative ksvc landmine (m5.7).
	envSkillRefs = "SKILL_REFS"

	// envSkillDescriptions is a JSON object of name → description. Descriptions ride in env
	// because they are ALWAYS-ON context anyway and are bounded (1024 bytes × 16 skills), so
	// the launcher can answer GET /skills with no I/O at all — the call an agent makes every
	// run must stay free, or progressive disclosure costs more than it saves.
	envSkillDescriptions = "SKILL_DESCRIPTIONS"

	// envSkillDir names the directory skill BODIES are mounted into, one file per skill.
	// Bodies are mounted rather than fetched on demand because a network call at the moment a
	// model asks for a skill is a latency spike mid-turn, and the content is already pinned by
	// digest so there is nothing to re-fetch. Mounting costs no CONTEXT, which is the scarce
	// resource progressive disclosure protects — disk is not.
	envSkillDir = "SKILL_DIR"

	// skillMountPath is where the bodies are mounted in the user container.
	skillMountPath = "/etc/agent/skills"

	// skillVolumeName is the pod volume name for the mounted skill ConfigMap.
	skillVolumeName = "agent-skills"
)

// injectSkills adds the SKILL_REFS env to the user container.
//
// Only the REFS travel this way, not the bodies. That is the progressive-disclosure contract
// made concrete: the launcher fetches a skill's description up front (cheap, and always in
// context) and its body only when the model asks for it. Mounting every attached body into the
// pod would defeat the entire point — the reason skills exist rather than a longer prompt is
// that context is scarce.
//
// It is also why this is env rather than a mounted ConfigMap like the prompt: a prompt is one
// blob that is always used, so caching it per revision is right; a skill's body may never be
// read at all in a given run.
func injectSkills(
	containers []corev1.Container,
	resolvedSkills []string,
	descriptions map[string]string,
	bodiesMounted bool,
) []corev1.Container {
	if len(resolvedSkills) == 0 || len(containers) == 0 {
		return containers
	}
	out := make([]corev1.Container, len(containers))
	copy(out, containers)
	// Index 0 is the user container by construction (buildPodTemplate builds it first).
	out[0].Env = upsertEnv(out[0].Env, corev1.EnvVar{
		Name:  envSkillRefs,
		Value: strings.Join(resolvedSkills, ","),
	})
	if len(descriptions) > 0 {
		// Marshal failure is impossible for map[string]string, and a nil value would be worse
		// than an empty one: the launcher tolerates a missing description but not a broken
		// env that takes the whole surface down.
		if b, err := json.Marshal(descriptions); err == nil {
			out[0].Env = upsertEnv(out[0].Env, corev1.EnvVar{
				Name: envSkillDescriptions, Value: string(b),
			})
		}
	}
	// SKILL_DIR is set ONLY when bodies are actually mounted. Setting it unconditionally would
	// make the launcher answer "no mounted body" with a 404 for content that was never staged,
	// instead of the 501 that says the capability is absent here.
	if bodiesMounted {
		out[0].Env = upsertEnv(out[0].Env, corev1.EnvVar{Name: envSkillDir, Value: skillMountPath})
		out[0].VolumeMounts = append(out[0].VolumeMounts, corev1.VolumeMount{
			Name:      skillVolumeName,
			MountPath: skillMountPath,
			ReadOnly:  true,
		})
	}
	return out
}

// upsertEnv sets name=value, replacing an existing entry rather than appending a duplicate —
// a duplicated env name is resolved by "last wins" in some runtimes and by an error in others,
// so neither is worth relying on.
func upsertEnv(env []corev1.EnvVar, e corev1.EnvVar) []corev1.EnvVar {
	for i := range env {
		if env[i].Name == e.Name {
			env[i] = e
			return env
		}
	}
	return append(env, e)
}
