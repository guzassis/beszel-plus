import { i18n } from "@lingui/core"
import { memo } from "react"
import { copyToClipboard, getHubURL } from "@/lib/utils"
import { DropdownMenuContent, DropdownMenuItem } from "./ui/dropdown-menu"

// const isbeta = beszel.hub_version.includes("beta")
// const imagetag = isbeta ? ":edge" : ""

/**
 * Get the URL of the script to install the agent.
 * @returns The URL for the script.
 */
const getScriptUrl = () => {
	return "https://raw.githubusercontent.com/guzassis/beszel-plus/main/supplemental/scripts/install-agent.sh"
	// no beta for now
	// const url = new URL("https://get.beszel.dev")
	// url.pathname = path
	// if (isBeta) {
	// 	url.searchParams.set("beta", "1")
	// }
	// return url.toString()
}

export type LinuxInstallOptions = {
	osUpdateManagement: boolean
	osUpdatePolicy: "monitor" | "security" | "official-all"
	powerManagement: boolean
	agentAutoUpdate: boolean
}

const defaultLinuxInstallOptions: LinuxInstallOptions = {
	osUpdateManagement: true,
	osUpdatePolicy: "security",
	powerManagement: true,
	agentAutoUpdate: true,
}

export function copyLinuxCommand(
	port = "45876",
	publicKey: string,
	token: string,
	options: LinuxInstallOptions = defaultLinuxInstallOptions
) {
	return copyToClipboard(buildLinuxCommand(port, publicKey, token, options))
}

export function buildLinuxCommand(
	port = "45876",
	publicKey: string,
	token: string,
	options: LinuxInstallOptions = defaultLinuxInstallOptions
) {
	let cmd = `curl -sL ${getScriptUrl()} -o /tmp/install-agent.sh && chmod +x /tmp/install-agent.sh && /tmp/install-agent.sh -p ${port} -k "${publicKey}" -t "${token}" -url "${getHubURL()}" --agent-auto-update=${options.agentAutoUpdate} --os-update-management=${options.osUpdateManagement} --os-update-policy=${options.osUpdatePolicy} --power-management=${options.powerManagement} --wait-for-apt=60`
	if ((i18n.locale + navigator.language).includes("zh-CN")) {
		cmd += ` --china-mirrors`
	}
	return cmd
}

export interface DropdownItem {
	text: string
	onClick?: () => void
	url?: string
	icons?: React.ComponentType<React.SVGProps<SVGSVGElement>>[]
}

export const InstallDropdown = memo(({ items }: { items: DropdownItem[] }) => {
	return (
		<DropdownMenuContent align="end">
			{items.map((item, index) => {
				const className = "cursor-pointer flex items-center gap-1.5"
				return item.url ? (
					<DropdownMenuItem key={index} asChild>
						<a href={item.url} className={className} target="_blank" rel="noopener noreferrer">
							{item.text}{" "}
							{item.icons?.map((Icon, iconIndex) => (
								<Icon key={iconIndex} className="size-4" />
							))}
						</a>
					</DropdownMenuItem>
				) : (
					<DropdownMenuItem key={index} onClick={item.onClick} className={className}>
						{item.text}{" "}
						{item.icons?.map((Icon, iconIndex) => (
							<Icon key={iconIndex} className="size-4" />
						))}
					</DropdownMenuItem>
				)
			})}
		</DropdownMenuContent>
	)
})
