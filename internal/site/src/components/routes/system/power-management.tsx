import { t } from "@lingui/core/macro"
import { Trans } from "@lingui/react/macro"
import { PowerIcon, SaveIcon } from "lucide-react"
import { useEffect, useState } from "react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { isAdmin, pb } from "@/lib/api"
import type { SystemRecord } from "@/types"
import { toast } from "@/components/ui/use-toast"

type Network = { id: string; name: string; interface: string; broadcast: string; enabled: boolean }
type PowerAPIResponse = { state?: string; status?: string }
const errorMessage = (error: unknown) => (error instanceof Error ? error.message : String(error))

function readinessLabel(state?: string) {
	switch (state) {
		case "ready":
			return t`Ready`
		case "disabled":
			return t`Disabled`
		case "unsupported":
			return t`Wake-on-LAN is not supported`
		case "no_physical_ethernet":
			return t`No physical Ethernet interface was found`
		case "no_carrier":
			return t`The Ethernet cable is disconnected`
		case "invalid_mac":
			return t`The network interface has an invalid MAC address`
		case "wol_not_enabled":
			return t`Wake-on-LAN is supported but disabled on the interface`
		case "ethtool_missing":
			return t`ethtool is not installed`
		case "network_unassigned":
			return t`The interface has no IPv4 address`
		default:
			return t`Unknown`
	}
}

function operationLabel(value?: string) {
	switch (value) {
		case "queued":
			return t`Queued`
		case "packet_sent":
			return t`Wake packet sent`
		case "already_online":
			return t`System is already online`
		case "completed":
			return t`Completed`
		default:
			return value ? t`Operation accepted` : t`Operation queued`
	}
}

export default function PowerManagement({ system }: { system: SystemRecord }) {
	const diagnostics = system.info.power
	const selectedInterface =
		diagnostics?.interfaces?.find((item) => item.interface === diagnostics.selected_interface) ??
		diagnostics?.interfaces?.[0]
	const powerEnabled = Boolean(system.power_management_enabled || diagnostics?.enabled)
	const wolReady = diagnostics?.state === "ready"
	const canConfigure = Boolean(diagnostics?.enabled)
	const [busy, setBusy] = useState(false)
	const [editing, setEditing] = useState(false)
	const [networks, setNetworks] = useState<Network[]>([])
	const [mac, setMac] = useState(system.wol_mac || selectedInterface?.mac || "")
	const [broadcast, setBroadcast] = useState(system.wol_broadcast || selectedInterface?.broadcast || "")
	const [networkId, setNetworkId] = useState(system.power_network_id || "")
	const [interfaceName, setInterfaceName] = useState(system.wol_interface || diagnostics?.selected_interface || "")
	const [customMinutes, setCustomMinutes] = useState("45")
	const hubHostname = (() => {
		try {
			return new URL(globalThis.BESZEL.HUB_URL).hostname
		} catch {
			return window.location.hostname
		}
	})()
	const targetHostname = system.host.startsWith("[")
		? system.host.slice(1, system.host.indexOf("]"))
		: system.host.split(":")[0]
	const isHubHost =
		["localhost", "127.0.0.1", "::1", hubHostname].includes(system.host) || hubHostname === targetHostname

	useEffect(() => {
		if (isAdmin())
			pb.send<{ networks: Network[] }>("/api/beszel/power/networks", {})
				.then((r) => setNetworks(r.networks))
				.catch(() => {})
	}, [])

	const stale = diagnostics?.collected_at
		? Date.now() - new Date(diagnostics.collected_at).getTime() > 86_400_000
		: false

	async function action(action: string, delaySeconds = 0) {
		if (action === "shutdown") {
			const warning = isHubHost
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

	async function save() {
		setBusy(true)
		try {
			await pb.collection("systems").update(system.id, {
				power_management_enabled: true,
				wol_enabled: wolReady,
				power_network_id: networkId,
				wol_interface: interfaceName,
				wol_mac: mac,
				wol_broadcast: broadcast,
				wol_port: 9,
			})
			toast({ title: t`Power settings saved` })
			setEditing(false)
		} catch (error: unknown) {
			toast({ title: t`Unable to save power settings`, description: errorMessage(error), variant: "destructive" })
		} finally {
			setBusy(false)
		}
	}

	return (
		<Card>
			<CardHeader className="pb-3">
				<CardTitle className="flex items-center gap-2 text-base">
					<PowerIcon className="size-4" />
					<Trans>Power management</Trans>
				</CardTitle>
			</CardHeader>
			<CardContent className="grid gap-3 text-sm">
				{!diagnostics ? (
					<p className="text-muted-foreground">
						<Trans>This Agent is older than v0.2.0 or does not expose power diagnostics.</Trans>
					</p>
				) : (
					<div>
						<span className="font-medium">
							<Trans>Readiness</Trans>:
						</span>{" "}
						{stale ? t`Stale` : readinessLabel(diagnostics.state)}
						{diagnostics.reason === "wol_probe_failed" ? ` — ${t`Unable to query Wake-on-LAN support`}` : ""}
					</div>
				)}
				{isHubHost ? (
					<p className="text-amber-600">
						<Trans>The Hub cannot wake itself. Use another Hub on the same power network.</Trans>
					</p>
				) : null}
				{isAdmin() && editing && (
					<div className="grid md:grid-cols-2 gap-2">
						{!canConfigure && (
							<p className="md:col-span-2 text-muted-foreground">
								<Trans>Power management is unavailable. The current controls are read-only.</Trans>
							</p>
						)}
						<select
							className="h-9 rounded-md border bg-background px-3"
							value={networkId}
							disabled={!canConfigure}
							onChange={(event) => {
								const selected = networks.find((network) => network.id === event.target.value)
								setNetworkId(event.target.value)
								if (selected) {
									setBroadcast(selected.broadcast)
									setInterfaceName(selected.interface)
								}
							}}
							aria-label={t`Power network`}
						>
							<option value="">
								<Trans>Broadcast override</Trans>
							</option>
							{networks
								.filter((network) => network.enabled)
								.map((network) => (
									<option key={network.id} value={network.id}>
										{network.name} — {network.broadcast}
									</option>
								))}
						</select>
						<Input
							value={mac}
							disabled={!canConfigure}
							onChange={(e) => setMac(e.target.value)}
							placeholder="02:11:22:33:44:55"
							aria-label={t`Wake-on-LAN MAC address`}
						/>
						<Input
							value={broadcast}
							disabled={!canConfigure}
							onChange={(e) => setBroadcast(e.target.value)}
							placeholder="192.168.1.255"
							aria-label={t`Broadcast address`}
							list="power-networks"
						/>
						<datalist id="power-networks">
							{networks
								.filter((n) => n.enabled)
								.map((n) => (
									<option key={n.id} value={n.broadcast}>
										{n.name}
									</option>
								))}
						</datalist>
					</div>
				)}
				{isAdmin() && (
					<div className="flex flex-wrap gap-2">
						<Button
							size="sm"
							variant="outline"
							disabled={busy || (editing && !canConfigure)}
							onClick={() => (editing ? save() : setEditing(true))}
						>
							{editing ? <SaveIcon className="size-4 me-1" /> : null}
							{editing ? t`Save configuration` : t`Configure`}
						</Button>
						{system.status === "down" && (
							<Button
								size="sm"
								disabled={busy || !system.wol_enabled || !wolReady || isHubHost}
								onClick={() => action("wake")}
							>
								<Trans>Wake system</Trans>
							</Button>
						)}
						{system.status === "up" && (
							<>
								<Button
									size="sm"
									variant="destructive"
									disabled={busy || !powerEnabled}
									onClick={() => action("shutdown", 3)}
								>
									<Trans>Shut down now</Trans>
								</Button>
								<Button
									size="sm"
									variant="outline"
									disabled={busy || !powerEnabled}
									onClick={() => action("shutdown", 900)}
								>
									<Trans>Shut down in 15 minutes</Trans>
								</Button>
								{[5, 30, 60].map((minutes) => (
									<Button
										key={minutes}
										size="sm"
										variant="outline"
										disabled={busy || !powerEnabled}
										onClick={() => action("shutdown", minutes * 60)}
									>
										{t`Shut down in ${minutes} minutes`}
									</Button>
								))}
								<Input
									className="h-8 w-24"
									type="number"
									min="1"
									max="10080"
									value={customMinutes}
									onChange={(event) => setCustomMinutes(event.target.value)}
									aria-label={t`Custom shutdown delay in minutes`}
								/>
								<Button
									size="sm"
									variant="outline"
									disabled={
										busy ||
										!powerEnabled ||
										!Number.isInteger(Number(customMinutes)) ||
										Number(customMinutes) < 1 ||
										Number(customMinutes) > 10080
									}
									onClick={() => action("shutdown", Number(customMinutes) * 60)}
								>
									<Trans>Schedule custom shutdown</Trans>
								</Button>
								<Button size="sm" variant="outline" disabled={busy} onClick={() => action("cancel-shutdown")}>
									<Trans>Cancel shutdown</Trans>
								</Button>
							</>
						)}
					</div>
				)}
			</CardContent>
		</Card>
	)
}
