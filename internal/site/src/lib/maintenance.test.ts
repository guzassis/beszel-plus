import { describe, expect, test } from "bun:test"
import { MAINTENANCE_PROTOCOL_VERSION } from "./maintenance"

describe("maintenance frontend contract", () => {
	test("uses the helper protocol version", () => {
		expect(MAINTENANCE_PROTOCOL_VERSION).toBe(2)
	})
})
