export const MAX_POWER_DELAY_SECONDS = 7 * 24 * 60 * 60
export const MIN_POWER_DELAY_SECONDS = 3

/** Convert a user-entered minute value into the whole seconds accepted by the API. */
export function parsePowerDelayMinutes(value: string): number | null {
	const normalized = value.trim().replace(",", ".")
	if (!/^(?:\d+(?:\.\d*)?|\.\d+)$/.test(normalized)) return null
	const minutes = Number(normalized)
	if (!Number.isFinite(minutes) || minutes <= 0) return null
	const seconds = Math.round(minutes * 60)
	if (seconds < MIN_POWER_DELAY_SECONDS || seconds > MAX_POWER_DELAY_SECONDS) return null
	return seconds
}

export function powerDelayMinutesLabel(seconds: number): string {
	return (seconds / 60).toLocaleString(undefined, { maximumFractionDigits: 2 })
}
