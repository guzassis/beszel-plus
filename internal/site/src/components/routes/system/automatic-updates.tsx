import { plural, t } from "@lingui/core/macro"
import { Trans } from "@lingui/react/macro"
import type { ReactNode } from "react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import { isAdmin, pb } from "@/lib/api"
import { cn } from "@/lib/utils"
import type {
	MaintenanceResponse,
	UpdateOverallState,
	UpdatePolicy,
	UpdatePolicyMode,
	UpdateRepository,
	UpdateStatus,
} from "@/types"

const stateColors: Partial<Record<UpdateOverallState, string>> = {
	healthy: "text-green-600 dark:text-green-400",
	updates_pending: "text-blue-600 dark:text-blue-400",
	update_in_progress: "text-blue-600 dark:text-blue-400",
	reboot_required: "text-orange-600 dark:text-orange-400",
	last_run_failed: "text-red-600 dark:text-red-400",
}

const defaultPolicy: UpdatePolicy = {
	enabled: true,
	mode: "security",
	update_package_lists_days: 1,
	unattended_upgrade_days: 1,
	automatic_reboot: false,
	automatic_reboot_time: "04:00",
	remove_unused_dependencies: false,
	allowed_repositories: [],
}

function stateLabel(state: UpdateOverallState) {
	switch (state) {
		case "healthy":
			return t`Up to date`
		case "updates_pending":
			return t`Updates pending`
		case "reboot_required":
			return t`Reboot required`
		case "last_run_failed":
			return t`Last upgrade failed`
		case "not_installed":
			return t`Not installed`
		case "installed_not_configured":
			return t`Installed, not configured`
		case "configured_disabled":
			return t`Automatic updates disabled`
		case "update_in_progress":
			return t`Updating`
		case "monitoring_incomplete":
			return t`Unavailable`
		case "unsupported":
			return t`Unsupported`
		default:
			return t`Unknown`
	}
}

function valueLabel(value: string) {
	const labels: Record<string, () => string> = {
		installed: () => t`Installed`,
		not_installed: () => t`Missing`,
		enabled: () => t`Enabled`,
		disabled: () => t`Disabled`,
		active: () => t`Active`,
		inactive: () => t`Inactive`,
		running: () => t`Running`,
		success: () => t`Success`,
		failed: () => t`Failed`,
		never_run: () => t`Never run`,
		partial: () => t`Partial`,
		missing: () => t`Missing`,
		not_found: () => t`Not found`,
		unknown: () => t`Unknown`,
	}
	return labels[value]?.() ?? value
}

function dateLabel(value?: string) {
	if (!value) return "—"
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString()
}
function Row({ label, children }: { label: ReactNode; children: ReactNode }) {
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
		const eligible = status.pending_updates_eligible ?? status.pending_updates ?? 0
		const updates = plural(eligible, { one: "# update", other: "# updates" })
		return status.pending_security_updates === undefined
			? updates
			: t`${updates} · ${status.pending_security_updates} security`
	}
	return stateLabel(status.overall_state)
}

function maintenance(systemId: string, operation: string, policy?: UpdatePolicy) {
	const requestId = crypto.randomUUID()
	return pb.send<MaintenanceResponse>("/api/beszel/maintenance", {
		method: "POST",
		body: {
			system_id: systemId,
			request: {
				version: 1,
				request_id: requestId,
				operation,
				idempotency_key: crypto.randomUUID(),
				...(policy ? { policy } : {}),
			},
		},
	})
}

function maintenanceError(response: MaintenanceResponse) {
	switch (response.error_code) {
		case "apt_lock_busy":
			return t`APT is currently busy. This operation will be retried automatically.`
		case "helper_incompatible":
			return t`Maintenance helper version is incompatible with this Agent. Re-run the Agent installer.`
		case "apt_config_invalid":
			return t`APT rejected the generated configuration.`
		case "apt_dry_run_failed":
			return t`APT dry-run failed after the configuration was validated.`
		case "timer_update_failed":
			return t`The update policy was restored because APT timers could not be updated.`
		default:
			return response.error || t`Operation failed`
	}
}

function PolicyDialog({
	systemId,
	status,
	open,
	setOpen,
}: {
	systemId: string
	status: UpdateStatus
	open: boolean
	setOpen: (value: boolean) => void
}) {
	const [policy, setPolicy] = useState<UpdatePolicy>(defaultPolicy)
	const [repositories, setRepositories] = useState<UpdateRepository[]>([])
	const [busy, setBusy] = useState(false)
	const [message, setMessage] = useState("")
	const [output, setOutput] = useState("")
	const capabilities = status.capabilities

	async function load() {
		setBusy(true)
		setMessage("")
		setOutput("")
		try {
			const [policyResponse, repositoryResponse] = await Promise.all([
				maintenance(systemId, "get-update-policy"),
				maintenance(systemId, "detect-repositories"),
			])
			if (policyResponse.result?.policy) setPolicy(policyResponse.result.policy)
			setRepositories(repositoryResponse.result?.repositories ?? [])
		} catch {
			setMessage(t`Unable to load update policy`)
		} finally {
			setBusy(false)
		}
	}

	async function run(operation: string, includePolicy = false) {
		setBusy(true)
		setMessage(t`Operation queued`)
		setOutput("")
		try {
			let response = await maintenance(systemId, operation, includePolicy ? policy : undefined)
			while (response.status === "queued" || response.status === "running") {
				setMessage(t`Operation in progress: ${response.progress ?? 0}%`)
				await new Promise((resolve) => setTimeout(resolve, 2000))
				response = await maintenance(systemId, "get-operation-status")
			}
			setMessage(response.status === "completed" ? t`Operation completed successfully` : maintenanceError(response))
			setOutput(response.result?.output ?? "")
		} catch {
			setMessage(t`Operation failed`)
		} finally {
			setBusy(false)
		}
	}

	function toggleRepository(repo: UpdateRepository, checked: boolean) {
		const selected = new Set(policy.allowed_repositories ?? [])
		if (checked) selected.add(repo.id)
		else selected.delete(repo.id)
		setPolicy({
			...policy,
			allowed_repositories: [...selected],
			confirm_third_party: [...selected].some(
				(id) => repositories.find((candidate) => candidate.id === id)?.official === false
			)
				? policy.confirm_third_party
				: false,
		})
	}

	return (
		<Dialog open={open} onOpenChange={setOpen}>
			<DialogContent className="max-w-2xl max-h-[90vh] overflow-auto" onOpenAutoFocus={load}>
				<DialogHeader>
					<DialogTitle>
						<Trans>Configure automatic updates</Trans>
					</DialogTitle>
					<DialogDescription>
						<Trans>The Agent remains unprivileged. A restricted local helper applies validated APT policies.</Trans>
					</DialogDescription>
				</DialogHeader>
				{!capabilities?.privileged_helper && (
					<p className="text-sm text-orange-600">
						<Trans>
							Privileged helper unavailable. Reinstall or upgrade the Agent with OS update management enabled.
						</Trans>
					</p>
				)}
				<div className="grid gap-4">
					<label htmlFor="package-list-frequency" className="grid gap-1 text-sm">
						<Trans>Update policy</Trans>
						<select
							className="h-9 rounded-md border bg-background px-3"
							value={policy.mode}
							onChange={(event) => setPolicy({ ...policy, mode: event.target.value as UpdatePolicyMode })}
						>
							<option value="monitor_only">{t`Monitoring only`}</option>
							<option value="security">{t`Security updates`}</option>
							<option value="official_all">{t`All official updates`}</option>
							<option value="custom">{t`Custom repositories`}</option>
						</select>
					</label>
					<div className="grid sm:grid-cols-2 gap-3">
						<label htmlFor="package-list-frequency" className="grid gap-1 text-sm">
							<Trans>Package list frequency (days)</Trans>
							<Input
								id="package-list-frequency"
								type="number"
								min={0}
								max={365}
								value={policy.update_package_lists_days}
								onChange={(event) => setPolicy({ ...policy, update_package_lists_days: Number(event.target.value) })}
							/>
						</label>
						<label htmlFor="automatic-update-frequency" className="grid gap-1 text-sm">
							<Trans>Automatic update frequency (days)</Trans>
							<Input
								id="automatic-update-frequency"
								type="number"
								min={0}
								max={365}
								value={policy.unattended_upgrade_days}
								onChange={(event) => setPolicy({ ...policy, unattended_upgrade_days: Number(event.target.value) })}
							/>
						</label>
					</div>
					<div className="flex items-center justify-between gap-3 text-sm">
						<span>
							<Trans>Remove unused dependencies</Trans>
						</span>
						<Switch
							aria-label={t`Remove unused dependencies`}
							checked={policy.remove_unused_dependencies}
							onCheckedChange={(checked) => setPolicy({ ...policy, remove_unused_dependencies: checked })}
						/>
					</div>
					<div className="flex items-center justify-between gap-3 text-sm">
						<span>
							<Trans>Automatic reboot</Trans>
						</span>
						<Switch
							aria-label={t`Automatic reboot`}
							checked={policy.automatic_reboot}
							onCheckedChange={(checked) => setPolicy({ ...policy, automatic_reboot: checked })}
						/>
					</div>
					{policy.automatic_reboot && (
						<>
							<p className="text-sm text-orange-600">
								<Trans>Automatic reboot can interrupt running services. It is disabled by default.</Trans>
							</p>
							<label htmlFor="automatic-reboot-time" className="grid gap-1 text-sm">
								<Trans>Automatic reboot time</Trans>
								<Input
									id="automatic-reboot-time"
									type="time"
									value={policy.automatic_reboot_time}
									onChange={(event) => setPolicy({ ...policy, automatic_reboot_time: event.target.value })}
								/>
							</label>
						</>
					)}
					{policy.mode === "custom" && (
						<div className="grid gap-2">
							<strong className="text-sm">
								<Trans>Detected repositories</Trans>
							</strong>
							{repositories.map((repo) => (
								<label key={repo.id} className="flex items-start gap-2 rounded border p-2 text-sm">
									<input
										type="checkbox"
										checked={policy.allowed_repositories?.includes(repo.id)}
										onChange={(event) => toggleRepository(repo, event.target.checked)}
									/>
									<span>
										<span className="font-medium">{repo.label || repo.origin}</span> · {repo.codename} ({repo.archive})
										<small className={cn("ms-2", repo.official ? "text-green-600" : "text-orange-600")}>
											{repo.official ? t`Official repository` : t`Third-party repository`}
										</small>
									</span>
								</label>
							))}
							{repositories.some((repo) => !repo.official && policy.allowed_repositories?.includes(repo.id)) && (
								<label className="flex items-center gap-2 text-sm text-orange-600">
									<input
										type="checkbox"
										checked={!!policy.confirm_third_party}
										onChange={(event) => setPolicy({ ...policy, confirm_third_party: event.target.checked })}
									/>
									<Trans>I explicitly confirm the selected third-party repositories</Trans>
								</label>
							)}
						</div>
					)}
					{message && <output className="text-sm">{message}</output>}
					{output && (
						<div className="grid gap-1">
							<strong className="text-sm">
								<Trans>Operation result</Trans>
							</strong>
							<pre className="max-h-48 overflow-auto whitespace-pre-wrap rounded bg-muted p-3 text-xs">{output}</pre>
						</div>
					)}
				</div>
				<DialogFooter className="flex-wrap">
					{status.installation_state !== "installed" && (
						<Button
							variant="outline"
							disabled={busy || !capabilities?.update_management}
							onClick={() => run("install-update-dependencies")}
						>
							<Trans>Install required dependencies</Trans>
						</Button>
					)}
					<Button variant="outline" disabled={busy} onClick={() => run("validate-update-policy", true)}>
						<Trans>Validate</Trans>
					</Button>
					<Button variant="outline" disabled={busy || !capabilities?.dry_run} onClick={() => run("run-update-dry-run")}>
						<Trans>Run dry-run</Trans>
					</Button>
					<Button disabled={busy || !capabilities?.policy_write} onClick={() => run("apply-update-policy", true)}>
						<Trans>Save and apply</Trans>
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	)
}

export default function AutomaticUpdates({ status, systemId }: { status?: UpdateStatus; systemId: string }) {
	const [open, setOpen] = useState(false)
	const [running, setRunning] = useState(false)
	const [message, setMessage] = useState("")
	if (!status) return null
	const caps = status.capabilities
	async function runNow() {
		if (!window.confirm(t`Run system updates now?`)) return
		setRunning(true)
		setMessage(t`Operation queued`)
		try {
			let response = await maintenance(systemId, "run-unattended-upgrades")
			while (response.status === "queued" || response.status === "running") {
				await new Promise((resolve) => setTimeout(resolve, 2000))
				response = await maintenance(systemId, "get-operation-status")
			}
			setMessage(response.status === "completed" ? t`Updates completed successfully` : maintenanceError(response))
		} catch {
			setMessage(t`Update failed`)
		} finally {
			setRunning(false)
		}
	}
	return (
		<Card>
			<CardHeader className="pb-3 flex-row items-center justify-between">
				<CardTitle>
					<Trans>Automatic updates</Trans>
				</CardTitle>
				<div className="flex gap-2">
					{isAdmin() && caps?.update_management && (
						<Button size="sm" variant="outline" onClick={() => setOpen(true)}>
							<Trans>Configure</Trans>
						</Button>
					)}
					{isAdmin() && caps?.run_upgrade && (
						<Button size="sm" disabled={running} onClick={runNow}>
							<Trans>Run updates now</Trans>
						</Button>
					)}
				</div>
			</CardHeader>
			<CardContent>
				<Row label={<Trans>Status</Trans>}>
					<strong className={cn(stateColors[status.overall_state])}>{updateSummary(status)}</strong>
				</Row>
				<Row label="unattended-upgrades">{valueLabel(status.installation_state)}</Row>
				<Row label={<Trans>Configuration</Trans>}>{valueLabel(status.configuration_state)}</Row>
				{status.timers?.map((timer) => (
					<Row key={timer.name} label={timer.name}>
						{valueLabel(timer.state)}
					</Row>
				))}
				<Row label={<Trans>Service</Trans>}>{valueLabel(status.service_state)}</Row>
				<Row label={<Trans>Available updates</Trans>}>{status.pending_updates_total ?? t`Unknown`}</Row>
				<Row label={<Trans>Eligible updates</Trans>}>{status.pending_updates_eligible ?? t`Unknown`}</Row>
				{status.eligibility_status === "busy" && (
					<Row label={<Trans>Eligibility status</Trans>}>
						<Trans>Eligibility is temporarily unavailable because APT is in use.</Trans>
					</Row>
				)}
				{status.eligibility_status === "stale" && (
					<Row label={<Trans>Eligibility status</Trans>}>
						<Trans>Showing the last successful eligibility result.</Trans>
					</Row>
				)}
				{status.eligibility_status === "unavailable" && (
					<Row label={<Trans>Eligibility status</Trans>}>
						<Trans>Eligibility is unavailable; other update information is still current.</Trans>
					</Row>
				)}
				<Row label={<Trans>Manual updates</Trans>}>{status.pending_updates_excluded ?? t`Unknown`}</Row>
				<Row label={<Trans>Security updates</Trans>}>{status.pending_security_updates ?? t`Unknown`}</Row>
				{!!status.excluded_repositories?.length && (
					<Row label={<Trans>Excluded repositories</Trans>}>{status.excluded_repositories.join(", ")}</Row>
				)}
				<Row label={<Trans>Last check</Trans>}>{dateLabel(status.last_check_at)}</Row>
				<Row label={<Trans>Last upgrade</Trans>}>{dateLabel(status.last_upgrade_at)}</Row>
				{!!status.last_upgrade_source && (
					<Row label={<Trans>Upgrade source</Trans>}>
						{status.last_upgrade_source === "manual" ? t`Manual` : t`Automatic`}
					</Row>
				)}
				<Row label={<Trans>Last result</Trans>}>{valueLabel(status.last_result)}</Row>
				<Row label={<Trans>Reboot required</Trans>}>{status.reboot_required ? t`Yes` : t`No`}</Row>
				{!!status.reboot_required_by?.length && (
					<Row label={<Trans>Reboot required by</Trans>}>{status.reboot_required_by.join(", ")}</Row>
				)}
				{!!status.recently_updated_packages?.length && (
					<Row label={<Trans>Recently updated</Trans>}>{status.recently_updated_packages.join(", ")}</Row>
				)}
				<Row label={<Trans>Collected</Trans>}>
					{dateLabel(status.collected_at)} ·{" "}
					{plural(status.cache_age_seconds, { one: "# second old", other: "# seconds old" })}
					{status.data_stale && (
						<>
							{" "}
							· <Trans>Stale data</Trans>
						</>
					)}
				</Row>
				{!!status.last_error && (
					<Row label={<Trans>Collection error</Trans>}>
						<span className="text-red-600 dark:text-red-400">{status.last_error}</span>
					</Row>
				)}
				{message && <output className="mt-3 text-sm">{message}</output>}
				<PolicyDialog systemId={systemId} status={status} open={open} setOpen={setOpen} />
			</CardContent>
		</Card>
	)
}
