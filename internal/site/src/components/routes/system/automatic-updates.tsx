import { Trans } from "@lingui/react/macro"
import type { ReactNode } from "react"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { cn } from "@/lib/utils"
import type { UpdateOverallState, UpdateStatus } from "@/types"

const stateLabels: Record<UpdateOverallState, string> = {
	healthy: "Up to date",
	updates_pending: "Updates pending",
	reboot_required: "Reboot required",
	last_run_failed: "Last upgrade failed",
	not_installed: "Not installed",
	installed_not_configured: "Installed, not configured",
	configured_disabled: "Automatic updates disabled",
	update_in_progress: "Updating",
	monitoring_incomplete: "Unavailable",
	unsupported: "Unsupported",
	unknown: "Unknown",
}

const stateColors: Partial<Record<UpdateOverallState, string>> = {
	healthy: "text-green-600 dark:text-green-400",
	updates_pending: "text-blue-600 dark:text-blue-400",
	update_in_progress: "text-blue-600 dark:text-blue-400",
	reboot_required: "text-orange-600 dark:text-orange-400",
	last_run_failed: "text-red-600 dark:text-red-400",
}

function dateLabel(value?: string) {
	if (!value) return "—"
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString()
}

function Row({ label, children }: { label: string; children: ReactNode }) {
	return (
		<div className="grid grid-cols-[minmax(9rem,1fr)_minmax(0,1.5fr)] gap-3 py-1.5 text-sm">
			<span className="text-muted-foreground">{label}</span>
			<span className="break-words">{children}</span>
		</div>
	)
}

export function updateSummary(status?: UpdateStatus) {
	if (!status) return "—"
	if (status.overall_state === "updates_pending") {
		const total = status.pending_updates ?? 0
		return status.pending_security_updates === undefined
			? `${total} updates`
			: `${total} updates · ${status.pending_security_updates} security`
	}
	return stateLabels[status.overall_state]
}

export default function AutomaticUpdates({ status }: { status?: UpdateStatus }) {
	if (!status) return null
	return (
		<Card>
			<CardHeader className="pb-3">
				<CardTitle>
					<Trans>Automatic updates</Trans>
				</CardTitle>
			</CardHeader>
			<CardContent>
				<Row label="Status">
					<strong className={cn(stateColors[status.overall_state])}>{updateSummary(status)}</strong>
				</Row>
				<Row label="unattended-upgrades">
					{status.installation_state === "installed"
						? "Installed"
						: status.installation_state === "not_installed"
							? "Missing"
							: "Unknown"}
				</Row>
				<Row label="Configuration">{status.configuration_state}</Row>
				{status.timers?.map((timer) => (
					<Row key={timer.name} label={timer.name}>
						{timer.state}
					</Row>
				))}
				<Row label="Service">{status.service_state}</Row>
				<Row label="Pending updates">{status.pending_updates ?? "Unknown"}</Row>
				<Row label="Security updates">{status.pending_security_updates ?? "Unknown"}</Row>
				<Row label="Last check">{dateLabel(status.last_check_at)}</Row>
				<Row label="Last upgrade">{dateLabel(status.last_upgrade_at)}</Row>
				<Row label="Last result">{status.last_result}</Row>
				<Row label="Reboot required">{status.reboot_required ? "Yes" : "No"}</Row>
				{!!status.reboot_required_by?.length && (
					<Row label="Reboot required by">{status.reboot_required_by.join(", ")}</Row>
				)}
				{!!status.recently_updated_packages?.length && (
					<Row label="Recently updated">{status.recently_updated_packages.join(", ")}</Row>
				)}
				<Row label="Collected">
					{dateLabel(status.collected_at)} ({status.cache_age_seconds}s old)
				</Row>
				{!!status.last_error && (
					<Row label="Collection error">
						<span className="text-red-600 dark:text-red-400">{status.last_error}</span>
					</Row>
				)}
			</CardContent>
		</Card>
	)
}
