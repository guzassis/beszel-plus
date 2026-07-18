import { describe, expect, test } from "bun:test"
import { MAX_POWER_DELAY_SECONDS, MIN_POWER_DELAY_SECONDS, parsePowerDelayMinutes } from "./power-actions"

describe("power delay conversion", () => {
	test("converts integer and decimal minutes", () => {
		expect(parsePowerDelayMinutes("1")).toBe(60)
		expect(parsePowerDelayMinutes("1.5")).toBe(90)
		expect(parsePowerDelayMinutes("1,5")).toBe(90)
	})

	test("rounds to whole seconds and enforces helper limits", () => {
		expect(parsePowerDelayMinutes("0.05")).toBe(MIN_POWER_DELAY_SECONDS)
		expect(parsePowerDelayMinutes("0.01")).toBeNull()
		expect(parsePowerDelayMinutes("10080")).toBe(MAX_POWER_DELAY_SECONDS)
		expect(parsePowerDelayMinutes("10080.1")).toBeNull()
	})

	test("rejects malformed values", () => {
		for (const value of ["", "0", "-1", "abc", "1.2.3", "Infinity", "1e2"]) {
			expect(parsePowerDelayMinutes(value)).toBeNull()
		}
	})
})
