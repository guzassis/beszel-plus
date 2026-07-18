import { t } from "@lingui/core/macro"
import { Trans, useLingui } from "@lingui/react/macro"
import { Clock3Icon, PowerIcon, PowerOffIcon } from "lucide-react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { isAdmin, pb } from "@/lib/api"
import { parsePowerDelayMinutes, powerDelayMinutesLabel } from "@/lib/power-actions"
import type { SystemRecord } from "@/types"
import { toast } from "../ui/use-toast"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "../ui/dropdown-menu"

type PowerAPIResponse = { state?: string; status?: string }

function errorMessage(error: unknown) {
	return error instanceof Error ? error.message : String(error)
}

function hubHostname() {
	try {
		return new URL(globalThis.BESZEL.HUB_URL).hostname
	} catch {
		return window.location.hostname
	}
}

function isHubSystem(system: SystemRecord) {
	const hostname = hubHostname()
	const target = system.host.startsWith("[")
		? system.host.slice(1, system.host.indexOf("]"))
		: system.host.split(":")[0]
	return ["localhost", "127.0.0.1", "::1", hostname].includes(system.host) || hostname === target
}

function operationLabel(value?: string) {
	switch (value) {
		case "packet_sent":
			return t`Wake packet sent`
		case "already_online":
			return t`System is already online`
		case "completed":
			return t`Completed`
		case "queued":
			return t`Queued`
		default:
			return value ? t`Operation accepted` : t`Operation queued`
	}
}

function wakeDisabledReason(status: SystemRecord["status"], wolReady: boolean, hubSystem: boolean) {
	if (status !== "down") return t`System is already online`
	if (hubSystem) return t`The Hub cannot wake this system`
	if (!wolReady) return t`Wake-on-LAN is not ready`
	return undefined
}

function shutdownDisabledReason(status: SystemRecord["status"], powerEnabled: boolean) {
	if (status !== "up") return t`System is offline`
	if (!powerEnabled) return t`Power management is disabled`
	return undefined
}

export const PowerActions = ({ system }: { system: SystemRecord }) => {
	const { t } = useLingui()
	const [busy, setBusy] = useState(false)
	const [scheduleOpen, setScheduleOpen] = useState(false)
	const [minutes, setMinutes] = useState("15")
	const [scheduleError, setScheduleError] = useState("")
	const diagnostics = system.info.power
	const powerEnabled = Boolean(system.power_management_enabled || diagnostics?.enabled)
	const wolReady = diagnostics?.state === "ready"
	const hubSystem = isHubSystem(system)
	const wakeDisabled = busy || system.status !== "down" || !wolReady || hubSystem
	const shutdownDisabled = busy || system.status !== "up" || !powerEnabled
	const wakeReason = wakeDisabledReason(system.status, wolReady, hubSystem)
	const shutdownReason = shutdownDisabledReason(system.status, powerEnabled)
	const parsedSeconds = parsePowerDelayMinutes(minutes)
	const scheduleDisabled = shutdownDisabled || parsedSeconds === null
	if (!isAdmin()) return null

	async function run(action: string, delaySeconds = 0, confirmed = false) {
		if (action === "shutdown" && !confirmed) {
			const warning = hubSystem
				? t`This system hosts Beszel Plus. Shutting it down will make the dashboard unavailable, and this Hub cannot wake itself. Continue?`
				: t`Confirm shutdown of this system?`
			if (!window.confirm(warning)) return
		}
		setBusy(true)
		try {
			const result = await pb.send<PowerAPIResponse>("/api/beszel/power", {
				method: "POST",
				body: { system_id: system.id, action, delay_seconds: delaySeconds },
			})
			toast({ title: t`Power operation`, description: operationLabel(result.state || result.status) })
		} catch (error: unknown) {
			toast({
				title: t`Power operation failed`,
				description: errorMessage(error) || t`Check system logs for details.`,
				variant: "destructive",
			})
		} finally {
			setBusy(false)
		}
	}

	function openSchedule() {
		setScheduleError("")
		setScheduleOpen(true)
	}

	function confirmSchedule() {
		if (parsedSeconds === null) {
			setScheduleError(t`Enter a positive duration between 3 seconds and 7 days.`)
			return
		}
		setScheduleOpen(false)
		run("shutdown", parsedSeconds, true).catch(console.error)
	}

	return (
		<>
			<DropdownMenu>
				<DropdownMenuTrigger asChild>
					<Button
						variant="ghost"
						size="icon"
						data-nolink
						aria-label={t`Power actions`}
						onClick={(event) => event.stopPropagation()}
						onMouseDown={(event) => event.stopPropagation()}
					>
						<PowerIcon className="size-[1.2em]" />
					</Button>
				</DropdownMenuTrigger>
				<DropdownMenuContent align="end" onClick={(event) => event.stopPropagation()}>
					<DropdownMenuItem
						disabled={wakeDisabled}
						title={wakeReason}
						onSelect={() => run("wake").catch(console.error)}
					>
						<PowerIcon className="me-2.5 size-4" />
						<Trans>Wake system</Trans>
					</DropdownMenuItem>
					<DropdownMenuItem
						disabled={shutdownDisabled}
						title={shutdownReason}
						onSelect={() => run("shutdown", 3).catch(console.error)}
					>
						<PowerOffIcon className="me-2.5 size-4" />
						<Trans>Shut down immediately</Trans>
					</DropdownMenuItem>
					<DropdownMenuItem disabled={scheduleDisabled} title={shutdownReason} onSelect={openSchedule}>
						<Clock3Icon className="me-2.5 size-4" />
						<Trans>Schedule shutdown</Trans>
					</DropdownMenuItem>
				</DropdownMenuContent>
			</DropdownMenu>
			<Dialog open={scheduleOpen} onOpenChange={setScheduleOpen}>
				<DialogContent className="w-[calc(100%-1rem)] max-h-[calc(100dvh-1rem)] overflow-y-auto overscroll-contain sm:max-w-md">
					<DialogHeader>
						<DialogTitle>
							<Trans>Schedule shutdown</Trans>
						</DialogTitle>
						<DialogDescription>
							<Trans>Choose how many minutes until {system.name} shuts down.</Trans>
						</DialogDescription>
					</DialogHeader>
					<label className="grid gap-1 text-sm" htmlFor={`power-delay-${system.id}`}>
						<Trans>Minutes</Trans>
						<Input
							id={`power-delay-${system.id}`}
							type="text"
							inputMode="decimal"
							value={minutes}
							onChange={(event) => {
								setMinutes(event.target.value)
								setScheduleError("")
							}}
							aria-invalid={Boolean(scheduleError)}
						/>
					</label>
					<p className="text-sm text-muted-foreground">
						{parsedSeconds === null
							? t`Use a value from 0.05 to 10080 minutes.`
							: t`This will schedule the shutdown in ${powerDelayMinutesLabel(parsedSeconds)} minutes (${parsedSeconds} seconds).`}
					</p>
					{scheduleError && <p className="text-sm text-destructive">{scheduleError}</p>}
					<DialogFooter>
						<Button variant="outline" onClick={() => setScheduleOpen(false)}>
							<Trans>Cancel</Trans>
						</Button>
						<Button disabled={parsedSeconds === null || busy} onClick={confirmSchedule}>
							<Trans>Confirm shutdown</Trans>
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>
		</>
	)
}

export default PowerActions
