import languages from "@/lib/languages"

export function normalizeLocale(detectedLocale?: string | null) {
	let locale = detectedLocale || "en"
	if (locale.startsWith("zh-")) {
		const zhVariantMap: Record<string, string> = {
			"zh-HK": "zh-HK",
			"zh-TW": "zh",
			"zh-MO": "zh",
			"zh-Hant": "zh",
		}
		return zhVariantMap[locale] || "zh-CN"
	}
	if (locale.toLowerCase() === "pt-br") return "pt-BR"
	if (locale.toLowerCase() === "pt-pt") return "pt"
	locale = locale.split("-")[0]
	return languages.some((language) => language[0] === locale) ? locale : "en"
}
