/** Runtime build identity injected by the Hub from internal/buildinfo. */
export const buildInfo = {
	productName: globalThis.BESZEL.PRODUCT_NAME || "Beszel Plus",
	version: globalThis.BESZEL.HUB_VERSION || "0.2.9-dev",
	repositoryUrl: globalThis.BESZEL.REPOSITORY_URL || "https://github.com/guzassis/beszel-plus",
	documentationUrl: globalThis.BESZEL.DOCUMENTATION_URL || "https://github.com/guzassis/beszel-plus#readme",
} as const

export const pageTitle = (title: string) => `${title} / ${buildInfo.productName}`
