export interface SystemEnrollmentClient<TSystem extends { id: string }> {
	createSystem(data: Record<string, unknown>): Promise<TSystem>
	createFingerprint(data: { system: string; token: string }): Promise<unknown>
	deleteSystem(id: string): Promise<unknown>
}

export async function persistSystemEnrollment<TSystem extends { id: string }>(
	client: SystemEnrollmentClient<TSystem>,
	data: Record<string, unknown>,
	token: string,
	afterPersist?: (system: TSystem) => Promise<void> | void
): Promise<TSystem> {
	const system = await client.createSystem(data)
	try {
		await client.createFingerprint({ system: system.id, token })
	} catch (error) {
		try {
			await client.deleteSystem(system.id)
		} catch (cleanupError) {
			throw new AggregateError(
				[error, cleanupError],
				"Fingerprint creation failed and the incomplete system could not be removed"
			)
		}
		throw error
	}
	await afterPersist?.(system)
	return system
}

export function createExclusiveAction<TArgs extends unknown[]>(
	action: (...args: TArgs) => Promise<void>
): (...args: TArgs) => Promise<boolean> {
	let running = false
	return async (...args: TArgs) => {
		if (running) {
			return false
		}
		running = true
		try {
			await action(...args)
			return true
		} finally {
			running = false
		}
	}
}
