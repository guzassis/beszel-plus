import type { MaintenanceCapabilities, UpdateCycleState, UpdatePolicy } from "@/types"

export const MAINTENANCE_PROTOCOL_VERSION = 3

export function supportsMaintenanceProtocol(protocolVersion?: number) {
	return (protocolVersion ?? 0) >= MAINTENANCE_PROTOCOL_VERSION
}

export function supportsUpdateCycles(capabilities?: MaintenanceCapabilities) {
	return Boolean(
		capabilities?.update_management &&
			capabilities.privileged_helper &&
			capabilities.update_cycle &&
			supportsMaintenanceProtocol(capabilities.protocol_version)
	)
}

export function isTerminalUpdateCycle(state?: UpdateCycleState) {
	return state === "completed" || state === "completed_with_pending" || state === "failed" || state === "canceled"
}

export function makeManualCycleFields(nextCycleId?: string, policyRevision?: string) {
	if (!nextCycleId || !policyRevision) return null
	return { cycle_id: nextCycleId, source: "manual" as const, policy_revision: policyRevision }
}

export function allowsManualUpdateCycle(policy?: UpdatePolicy) {
	return Boolean(policy?.enabled && policy.mode !== "monitor_only")
}
