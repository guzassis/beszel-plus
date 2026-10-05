import { describe, expect, test } from "bun:test"
import {
	allowsManualUpdateCycle,
	isTerminalUpdateCycle,
	MAINTENANCE_PROTOCOL_VERSION,
	makeManualCycleFields,
	supportsMaintenanceProtocol,
	supportsUpdateCycles,
} from "./maintenance"

describe("maintenance frontend contract", () => {
	test("uses the helper protocol version", () => {
		expect(MAINTENANCE_PROTOCOL_VERSION).toBe(3)
		expect(supportsMaintenanceProtocol(3)).toBe(true)
		expect(supportsMaintenanceProtocol(2)).toBe(false)
		expect(supportsMaintenanceProtocol(undefined)).toBe(false)
	})

	test("requires protocol 3 and the dedicated helper capability for update cycles", () => {
		expect(
			supportsUpdateCycles({
				update_management: true,
				privileged_helper: true,
				update_cycle: true,
				protocol_version: 3,
			})
		).toBe(true)
		expect(
			supportsUpdateCycles({
				update_management: true,
				privileged_helper: true,
				update_cycle: true,
				protocol_version: 2,
			})
		).toBe(false)
		expect(supportsUpdateCycles({ update_management: true, privileged_helper: true, protocol_version: 3 })).toBe(false)
	})

	test("builds a manual cycle request from the helper's next ID and current policy revision", () => {
		expect(makeManualCycleFields("cycle-00000000000000000007", "rev-a")).toEqual({
			cycle_id: "cycle-00000000000000000007",
			source: "manual",
			policy_revision: "rev-a",
		})
		expect(makeManualCycleFields(undefined, "rev-a")).toBeNull()
		expect(makeManualCycleFields("cycle-1", undefined)).toBeNull()
	})

	test("treats pending completion as terminal without turning unknown into a count", () => {
		expect(isTerminalUpdateCycle("completed_with_pending")).toBe(true)
		expect(isTerminalUpdateCycle("running")).toBe(false)
		expect(isTerminalUpdateCycle(undefined)).toBe(false)
	})

	test("allows manual cycles only when an enabled update policy is known", () => {
		const base = {
			update_package_lists_days: 1,
			unattended_upgrade_days: 1,
			automatic_reboot: false,
			automatic_reboot_time: "04:00",
			remove_unused_dependencies: false,
		}
		expect(allowsManualUpdateCycle({ ...base, enabled: true, mode: "official_all" })).toBe(true)
		expect(allowsManualUpdateCycle({ ...base, enabled: true, mode: "security" })).toBe(true)
		expect(allowsManualUpdateCycle({ ...base, enabled: true, mode: "custom" })).toBe(true)
		expect(allowsManualUpdateCycle({ ...base, enabled: true, mode: "monitor_only" })).toBe(false)
		expect(allowsManualUpdateCycle({ ...base, enabled: false, mode: "official_all" })).toBe(false)
		expect(allowsManualUpdateCycle(undefined)).toBe(false)
	})
})
