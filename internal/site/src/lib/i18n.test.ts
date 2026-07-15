import { describe, expect, test } from "bun:test"
import { normalizeLocale } from "./locale"

describe("locale normalization", () => {
	test("keeps Brazilian and European Portuguese distinct", () => {
		expect(normalizeLocale("pt-BR")).toBe("pt-BR")
		expect(normalizeLocale("pt-br")).toBe("pt-BR")
		expect(normalizeLocale("pt-PT")).toBe("pt")
	})
})
