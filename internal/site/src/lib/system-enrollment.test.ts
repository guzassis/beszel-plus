import { describe, expect, test } from "bun:test"
import { createExclusiveAction, persistSystemEnrollment, type SystemEnrollmentClient } from "./system-enrollment"

type TestSystem = { id: string }

function clientWith(events: string[], fingerprintError?: Error): SystemEnrollmentClient<TestSystem> {
	return {
		createSystem() {
			events.push("system")
			return Promise.resolve({ id: "system-1" })
		},
		createFingerprint() {
			events.push("fingerprint")
			if (fingerprintError) return Promise.reject(fingerprintError)
			return Promise.resolve()
		},
		deleteSystem() {
			events.push("cleanup")
			return Promise.resolve()
		},
	}
}

describe("system enrollment", () => {
	test("copies only after the system and fingerprint are persisted", async () => {
		const events: string[] = []
		await persistSystemEnrollment(clientWith(events), { name: "host" }, "token", () => {
			events.push("copy")
		})
		expect(events).toEqual(["system", "fingerprint", "copy"])
	})

	test("removes an incomplete system when fingerprint creation fails", async () => {
		const events: string[] = []
		await expect(
			persistSystemEnrollment(clientWith(events, new Error("fingerprint failed")), {}, "token")
		).rejects.toThrow("fingerprint failed")
		expect(events).toEqual(["system", "fingerprint", "cleanup"])
	})

	test("coalesces concurrent actions without creating duplicates", async () => {
		let release: (() => void) | undefined
		const pending = new Promise<void>((resolve) => {
			release = resolve
		})
		let calls = 0
		const action = createExclusiveAction(async () => {
			calls++
			await pending
		})
		const first = action()
		expect(await action()).toBe(false)
		expect(calls).toBe(1)
		release?.()
		expect(await first).toBe(true)
	})
})
