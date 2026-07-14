import { t } from "@lingui/core/macro"
import { Trans } from "@lingui/react/macro"
import { useStore } from "@nanostores/react"
import { getPagePath } from "@nanostores/router"
import { ChevronDownIcon, ExternalLinkIcon } from "lucide-react"
import { memo, useEffect, useRef, useState } from "react"
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
import { Label } from "@/components/ui/label"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { isReadOnlyUser, pb } from "@/lib/api"
import { SystemStatus } from "@/lib/enums"
import { $publicKey } from "@/lib/stores"
import { cn, generateToken, tokenMap, useBrowserStorage } from "@/lib/utils"
import type { SystemRecord } from "@/types"
import { copyLinuxCommand, type DropdownItem, InstallDropdown, type LinuxInstallOptions } from "./install-dropdowns"
import { $router, basePath, Link, navigate } from "./router"
import { DropdownMenu, DropdownMenuTrigger } from "./ui/dropdown-menu"
import { TuxIcon } from "./ui/icons"
import { InputCopy } from "./ui/input-copy"

// To avoid a refactor of the dialog, we will just keep this function as a "skeleton" for the actual dialog
export function AddSystemDialog({ open, setOpen }: { open: boolean; setOpen: (open: boolean) => void }) {
	const opened = useRef(false)
	if (open) {
		opened.current = true
	}

	if (isReadOnlyUser()) {
		return null
	}

	return (
		<Dialog open={open} onOpenChange={setOpen}>
			{opened.current && <SystemDialog setOpen={setOpen} />}
		</Dialog>
	)
}

/**
 * Token to be used for the next system.
 * Prevents token changing if user copies config, then closes dialog and opens again.
 */
let nextSystemToken: string | null = null

/**
 * SystemDialog component for adding or editing a system.
 * @param {Object} props - The component props.
 * @param {function} props.setOpen - Function to set the open state of the dialog.
 * @param {SystemRecord} [props.system] - Optional system record for editing an existing system.
 */
export const SystemDialog = ({ setOpen, system }: { setOpen: (open: boolean) => void; system?: SystemRecord }) => {
	const publicKey = useStore($publicKey)
	const port = useRef<HTMLInputElement>(null)
	const [hostValue, setHostValue] = useState(system?.host ?? "")
	const isUnixSocket = hostValue.startsWith("/")
	const [, setTab] = useBrowserStorage("as-tab", "binary")
	const [token, setToken] = useState(system?.token ?? "")
	const [installOptions, setInstallOptions] = useState<LinuxInstallOptions>({
		osUpdateManagement: true,
		osUpdatePolicy: "security",
		powerManagement: true,
		agentAutoUpdate: true,
	})

	useEffect(() => {
		;(async () => {
			// if no system, generate a new token
			if (!system) {
				nextSystemToken ||= generateToken()
				return setToken(nextSystemToken)
			}
			// if system exists,get the token from the fingerprint record
			if (tokenMap.has(system.id)) {
				return setToken(tokenMap.get(system.id)!)
			}
			const { token } = await pb.collection("fingerprints").getFirstListItem(`system = "${system.id}"`, {
				fields: "token",
			})
			tokenMap.set(system.id, token)
			setToken(token)
		})()
	}, [system?.id, nextSystemToken])

	async function handleSubmit(e: SubmitEvent) {
		e.preventDefault()
		const formData = new FormData(e.target as HTMLFormElement)
		const data = Object.fromEntries(formData) as Record<string, any>
		data.users = pb.authStore.record!.id
		try {
			setOpen(false)
			if (system) {
				await pb.collection("systems").update(system.id, { ...data, status: SystemStatus.Pending })
			} else {
				const createdSystem = await pb.collection("systems").create(data)
				await pb.collection("fingerprints").create({
					system: createdSystem.id,
					token,
				})
				// Reset the current token after successful system
				// creation so next system gets a new token
				nextSystemToken = null
			}
			navigate(basePath)
		} catch (e) {
			console.error(e)
		}
	}

	const systemTranslation = t`System`

	return (
		<DialogContent
			className="w-[90%] sm:w-auto sm:ns-dialog max-w-full rounded-lg"
			onCloseAutoFocus={() => {
				setHostValue(system?.host ?? "")
			}}
		>
			<Tabs value="binary" onValueChange={setTab}>
				<DialogHeader>
					<DialogTitle className="mb-1 pb-1 max-w-100 truncate pr-8">
						{system ? (
							<Trans>Edit {{ foo: systemTranslation }}</Trans>
						) : (
							<Trans>Add {{ foo: systemTranslation }}</Trans>
						)}
					</DialogTitle>
					<TabsList className="grid w-full grid-cols-1">
						<TabsTrigger value="binary">
							<Trans>Binary</Trans>
						</TabsTrigger>
					</TabsList>
				</DialogHeader>
				{/* Binary */}
				<TabsContent value="binary" tabIndex={-1}>
					<DialogDescription className="mb-3 leading-relaxed w-0 min-w-full">
						<Trans>
							Copy the installation command for the agent below, or register agents automatically with a{" "}
							<Link
								onClick={() => setOpen(false)}
								href={getPagePath($router, "settings", { name: "tokens" })}
								className="link"
							>
								universal token
							</Link>
							.
						</Trans>
					</DialogDescription>
				</TabsContent>
				<form onSubmit={handleSubmit as any}>
					<div className="grid xs:grid-cols-[auto_1fr] gap-y-3 gap-x-4 items-center mt-1 mb-4">
						<Label htmlFor="name" className="xs:text-end">
							<Trans>Name</Trans>
						</Label>
						<Input id="name" name="name" defaultValue={system?.name} required />
						<Label htmlFor="host" className="xs:text-end">
							<Trans>Host / IP</Trans>
						</Label>
						<Input
							id="host"
							name="host"
							value={hostValue}
							required
							onChange={(e) => {
								setHostValue(e.target.value)
							}}
						/>
						<Label htmlFor="port" className={cn("xs:text-end", isUnixSocket && "hidden")}>
							<Trans>Port</Trans>
						</Label>
						<Input
							ref={port}
							name="port"
							id="port"
							defaultValue={system?.port || "45876"}
							required={!isUnixSocket}
							className={cn(isUnixSocket && "hidden")}
						/>
						<Label htmlFor="pkey" className="xs:text-end whitespace-pre">
							<Trans comment="Use 'Key' if your language requires many more characters">Public Key</Trans>
						</Label>
						<InputCopy value={publicKey} id="pkey" name="pkey" />
						<Label htmlFor="tkn" className="xs:text-end whitespace-pre">
							<Trans>Token</Trans>
						</Label>
						<InputCopy value={token} id="tkn" name="tkn" />
						<div className="xs:col-span-2 rounded-md border p-3 grid gap-2 text-sm">
							<strong>
								<Trans>Agent installation options</Trans>
							</strong>
							<label className="flex items-center gap-2">
								<input
									type="checkbox"
									checked={installOptions.osUpdateManagement}
									onChange={(event) =>
										setInstallOptions({ ...installOptions, osUpdateManagement: event.target.checked })
									}
								/>
								<Trans>System update management</Trans>
							</label>
							<label className="flex items-center gap-2">
								<span>
									<Trans>Initial update policy</Trans>
								</span>
								<select
									className="h-8 rounded-md border bg-background px-2"
									value={installOptions.osUpdatePolicy}
									disabled={!installOptions.osUpdateManagement}
									onChange={(event) =>
										setInstallOptions({
											...installOptions,
											osUpdatePolicy: event.target.value as LinuxInstallOptions["osUpdatePolicy"],
										})
									}
								>
									<option value="monitor">Monitor</option>
									<option value="security">Security</option>
									<option value="official-all">Official repositories</option>
								</select>
							</label>
							<label className="flex items-center gap-2">
								<input
									type="checkbox"
									checked={installOptions.powerManagement}
									onChange={(event) => setInstallOptions({ ...installOptions, powerManagement: event.target.checked })}
								/>
								<Trans>Power management</Trans>
							</label>
							<label className="flex items-center gap-2">
								<input
									type="checkbox"
									checked={installOptions.agentAutoUpdate}
									onChange={(event) => setInstallOptions({ ...installOptions, agentAutoUpdate: event.target.checked })}
								/>
								<Trans>Automatic Agent updates</Trans>
							</label>
						</div>
					</div>
					<DialogFooter className="flex justify-end gap-x-2 gap-y-3 flex-col mt-5">
						{/* Binary */}
						<TabsContent value="binary" className="contents">
							<CopyButton
								text={t`Copy Linux command`}
								icon={<TuxIcon className="size-4" />}
								onClick={async () =>
									copyLinuxCommand(isUnixSocket ? hostValue : port.current?.value, publicKey, token, installOptions)
								}
								dropdownItems={[
									{
										text: t`Manual setup instructions`,
										url: "https://github.com/guzassis/beszel-plus#installation",
										icons: [ExternalLinkIcon],
									},
								]}
							/>
						</TabsContent>
						{/* Save */}
						<Button>
							{system ? (
								<Trans>Save {{ foo: systemTranslation }}</Trans>
							) : (
								<Trans>Add {{ foo: systemTranslation }}</Trans>
							)}
						</Button>
					</DialogFooter>
				</form>
			</Tabs>
		</DialogContent>
	)
}

interface CopyButtonProps {
	text: string
	onClick: () => void
	dropdownItems: DropdownItem[]
	icon?: React.ReactElement<any>
}

const CopyButton = memo((props: CopyButtonProps) => {
	return (
		<div className="flex gap-0 rounded-lg">
			<Button
				type="button"
				variant="outline"
				onClick={props.onClick}
				className="rounded-e-none dark:border-e-0 grow flex items-center gap-2"
			>
				{props.text} {props.icon}
			</Button>
			<div className="w-px h-full bg-muted"></div>
			<DropdownMenu>
				<DropdownMenuTrigger asChild>
					<Button variant="outline" className={"px-2 rounded-s-none border-s-0"}>
						<ChevronDownIcon />
					</Button>
				</DropdownMenuTrigger>
				<InstallDropdown items={props.dropdownItems} />
			</DropdownMenu>
		</div>
	)
})
