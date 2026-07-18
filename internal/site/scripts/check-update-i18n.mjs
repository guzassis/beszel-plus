import { readFileSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const root = join(dirname(fileURLToPath(import.meta.url)), "..")
const locales = [
	"ar",
	"bg",
	"cs",
	"da",
	"de",
	"en",
	"es",
	"fa",
	"fr",
	"he",
	"hr",
	"hu",
	"id",
	"it",
	"ja",
	"ko",
	"nl",
	"no",
	"pl",
	"pt",
	"ru",
	"sl",
	"sr",
	"sv",
	"tr",
	"uk",
	"vi",
	"zh",
	"zh-CN",
	"zh-HK",
]
const pt = {
	"{0, plural, one {# second old} other {# seconds old}}": "{0, plural, one {há # segundo} other {há # segundos}}",
	"{eligible, plural, one {# update} other {# updates}}":
		"{eligible, plural, one {# atualização} other {# atualizações}}",
	"{updates} · {0} security": "{updates} · {0} de segurança",
	"{0} · {1}": "{0} · {1}",
	"All official updates": "Todas as atualizações oficiais",
	"Automatic reboot": "Reinicialização automática",
	"Automatic reboot can interrupt running services. It is disabled by default.":
		"A reinicialização automática pode interromper serviços em execução. Ela fica desativada por padrão.",
	"Automatic reboot time": "Horário da reinicialização automática",
	"Automatic update failed": "A atualização automática falhou",
	"Automatic update frequency (days)": "Frequência das atualizações automáticas (dias)",
	"Automatic updates": "Atualizações automáticas",
	Automatic: "Automática",
	"Automatic updates disabled": "Atualizações automáticas desativadas",
	"Available updates": "Atualizações disponíveis",
	Collected: "Coletado",
	"Collection error": "Erro de coleta",
	Configuration: "Configuração",
	Configure: "Configurar",
	"Configure automatic updates": "Configurar atualizações automáticas",
	"Custom repositories": "Repositórios personalizados",
	"Detected repositories": "Repositórios detectados",
	Disabled: "Desativado",
	"Eligible updates": "Atualizações elegíveis",
	Enabled: "Ativado",
	"Excluded repositories": "Repositórios excluídos",
	"I explicitly confirm the selected third-party repositories":
		"Confirmo explicitamente os repositórios de terceiros selecionados",
	"Install required dependencies": "Instalar dependências necessárias",
	Installed: "Instalado",
	"Installed, not configured": "Instalado, não configurado",
	"Last check": "Última verificação",
	"Last result": "Último resultado",
	"Last update failed": "A última atualização falhou",
	"Last upgrade": "Última atualização",
	"Last upgrade failed": "A última atualização falhou",
	"Manual updates": "Atualizações manuais",
	Manual: "Manual",
	Missing: "Ausente",
	"Monitoring only": "Somente monitoramento",
	"Never run": "Nunca executado",
	"Not found": "Não encontrado",
	"Not installed": "Não instalado",
	"Official repository": "Repositório oficial",
	"Operation completed successfully": "Operação concluída com sucesso",
	"Operation failed": "A operação falhou",
	"Operation result": "Resultado da operação",
	"Operation in progress: {0}%": "Operação em andamento: {0}%",
	"Operation queued": "Operação enfileirada",
	"Package list frequency (days)": "Frequência da lista de pacotes (dias)",
	"Package missing": "Pacote ausente",
	Partial: "Parcial",
	"Privileged helper unavailable. Reinstall or upgrade the Agent with OS update management enabled.":
		"O helper privilegiado não está disponível. Reinstale ou atualize o Agent com o gerenciamento de atualizações do sistema ativado.",
	"Reboot required": "Reinicialização necessária",
	"Reboot required by": "Reinicialização exigida por",
	"Recently updated": "Atualizados recentemente",
	"Remove unused dependencies": "Remover dependências não utilizadas",
	"Run dry-run": "Executar simulação",
	"Run system updates now?": "Executar as atualizações do sistema agora?",
	"Run updates now": "Executar atualizações agora",
	Running: "Em execução",
	"Save and apply": "Salvar e aplicar",
	"Security updates": "Atualizações de segurança",
	"Security updates pending": "Atualizações de segurança pendentes",
	Service: "Serviço",
	"Stale data": "Dados desatualizados",
	Success: "Sucesso",
	"The Agent remains unprivileged. A restricted local helper applies validated APT policies.":
		"O Agent continua sem privilégios. Um helper local restrito aplica políticas do APT previamente validadas.",
	"Third-party repository": "Repositório de terceiros",
	"Triggers when a reboot is required": "Dispara quando uma reinicialização é necessária",
	"Triggers when a reboot remains required longer than the threshold":
		"Dispara quando a reinicialização permanece necessária além do limite",
	"Triggers when automatic updates are disabled": "Dispara quando as atualizações automáticas estão desativadas",
	"Triggers when security updates are pending": "Dispara quando há atualizações de segurança pendentes",
	"Triggers when security updates remain pending longer than the threshold":
		"Dispara quando as atualizações de segurança permanecem pendentes além do limite",
	"Triggers when the last automatic update failed": "Dispara quando a última atualização automática falhou",
	"Triggers when unattended-upgrades is not installed": "Dispara quando o unattended-upgrades não está instalado",
	"Triggers when update information is older than the threshold":
		"Dispara quando as informações de atualização estão mais antigas que o limite",
	"Unable to load update policy": "Não foi possível carregar a política de atualizações",
	"Unattended upgrades package missing": "Pacote unattended-upgrades ausente",
	Unavailable: "Indisponível",
	Unsupported: "Não suportado",
	"Up to date": "Atualizado",
	"Update failed": "A atualização falhou",
	"Update information stale": "Informações de atualização desatualizadas",
	"Update policy": "Política de atualizações",
	"Upgrade source": "Origem da atualização",
	"Updates completed successfully": "Atualizações concluídas com sucesso",
	"Updates pending": "Atualizações pendentes",
	Updating: "Atualizando",
	Validate: "Validar",
	"Apply update policy": "Aplicar política de atualizações",
	"Validate update policy": "Validar política de atualizações",
	"Install update dependencies": "Instalar dependências de atualização",
	"Run update dry-run": "Executar simulação de atualização",
	"Run system updates": "Executar atualizações do sistema",
	"Read update policy": "Ler política de atualizações",
	"Detect repositories": "Detectar repositórios",
	"Read operation status": "Ler status da operação",
	Queued: "Enfileirada",
	Completed: "Concluída",
	Event: "Evento",
	"Maintenance event": "Evento de manutenção",
	"Update package not installed": "Pacote de atualização não instalado",
	"Updates available": "Atualizações disponíveis",
	"Security updates available": "Atualizações de segurança disponíveis",
	"Upgrade started": "Atualização iniciada",
	"Upgrade completed": "Atualização concluída",
	"Upgrade failed": "A atualização falhou",
	"Reboot requirement cleared": "Necessidade de reinicialização removida",
	"Update monitoring error": "Erro no monitoramento de atualizações",
	"Maintenance operation refused": "Operação de manutenção recusada",
	"Unauthorized maintenance attempt": "Tentativa de manutenção não autorizada",
	"Maintenance helper incompatible": "Helper de manutenção incompatível",
	"Agent incompatible with maintenance operations": "Agent incompatível com operações de manutenção",
	"Maintenance operation timed out": "A operação de manutenção excedeu o tempo limite",
	Ready: "Pronto",
	"Wake-on-LAN is not supported": "Wake-on-LAN não é suportado",
	"No physical Ethernet interface was found": "Nenhuma interface Ethernet física foi encontrada",
	"The Ethernet cable is disconnected": "O cabo Ethernet está desconectado",
	"The network interface has an invalid MAC address": "A interface de rede tem um endereço MAC inválido",
	"Wake-on-LAN is supported but disabled on the interface": "Wake-on-LAN é suportado, mas está desativado na interface",
	"ethtool is not installed": "O ethtool não está instalado",
	"The interface has no IPv4 address": "A interface não possui endereço IPv4",
	"Wake packet sent": "Pacote de ativação enviado",
	"System is already online": "O sistema já está online",
	"Operation accepted": "Operação aceita",
	Stale: "Desatualizado",
	"Unable to query Wake-on-LAN support": "Não foi possível consultar o suporte a Wake-on-LAN",
	"Power management is unavailable. The current controls are read-only.":
		"O gerenciamento de energia está indisponível. Os controles atuais são somente leitura.",
	"Update management is unavailable. The current controls are read-only.":
		"O gerenciamento de atualizações está indisponível. Os controles atuais são somente leitura.",
	"Collecting update information…": "Coletando informações de atualização…",
	"Waiting for the first collection": "Aguardando a primeira coleta",
	"Official repositories": "Repositórios oficiais",
	"Power actions": "Ações de energia",
	"Power operation": "Operação de energia",
	"Power operation failed": "Falha na operação de energia",
	"The Hub cannot wake this system": "O Hub não pode acordar este sistema",
	"Wake-on-LAN is not ready": "O Wake-on-LAN não está pronto",
	"Wake-on-LAN is ready": "O Wake-on-LAN está pronto",
	"Wake-on-LAN diagnostics are unavailable": "Os diagnósticos do Wake-on-LAN estão indisponíveis",
	"The Wake-on-LAN probe failed": "A verificação do Wake-on-LAN falhou",
	"ethtool is unavailable to the Agent service": "O ethtool está indisponível para o serviço do Agent",
	"Power diagnostics are disabled": "Os diagnósticos de energia estão desativados",
	"System is offline": "O sistema está desligado",
	"Power management is disabled": "O gerenciamento de energia está desativado",
	"This system hosts Beszel Plus. Shutting it down will make the dashboard unavailable, and this Hub cannot wake itself. Continue?":
		"Este sistema hospeda o Beszel Plus. Desligá-lo tornará o painel indisponível, e este Hub não pode acordá-lo. Continuar?",
	"Confirm shutdown of this system?": "Confirmar desligamento deste sistema?",
	"Enter a positive duration between 3 seconds and 7 days.": "Informe uma duração positiva entre 3 segundos e 7 dias.",
	"Wake system": "Acordar sistema",
	"Shut down immediately": "Desligar imediatamente",
	"Schedule shutdown": "Agendar desligamento",
	"Choose how many minutes until {0} shuts down.": "Escolha quantos minutos até {0} ser desligado.",
	Minutes: "Minutos",
	"Use a value from 0.05 to 10080 minutes.": "Use um valor de 0,05 a 10080 minutos.",
	"This will schedule the shutdown in {0} minutes ({parsedSeconds} seconds).":
		"O desligamento será agendado para daqui a {0} minutos ({parsedSeconds} segundos).",
	"Confirm shutdown": "Confirmar desligamento",
}

const featureReference =
	/automatic-updates\.tsx|power-actions\.tsx|add-system\.tsx|alerts-history-columns\.tsx|src\/lib\/alerts\.ts/
const decode = (value) => JSON.parse(`"${value.replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`)
const placeholders = (value) =>
	[...value.matchAll(/\{([A-Za-z0-9_]+)(?:,|\})/g)]
		.map((match) => match[1])
		.sort()
		.join("|")
const fix = process.argv.includes("--fix")
let failed = false

for (const locale of locales) {
	const path = join(root, "src", "locales", locale, `${locale}.po`)
	const source = readFileSync(path, "utf8")
	const blocks = source.split("\n\n").map((block) => {
		if (!featureReference.test(block)) return block
		const idMatch = block.match(/^msgid "([^"]*)"$/m)
		const valueMatch = block.match(/^msgstr "([^"]*)"$/m)
		if (!idMatch || !valueMatch) return block
		const id = decode(idMatch[1])
		let value = decode(valueMatch[1])
		if (!value && fix) {
			value = locale === "pt" ? pt[id] : id
			if (!value) throw new Error(`Missing Portuguese translation: ${id}`)
			block = block.replace(/^msgstr ""$/m, `msgstr ${JSON.stringify(value)}`)
		}
		if (!value) {
			console.error(`${locale}: empty update translation: ${id}`)
			failed = true
		}
		if (value && placeholders(id) !== placeholders(value)) {
			console.error(`${locale}: incompatible placeholders: ${id}`)
			failed = true
		}
		return block
	})
	const next = blocks.join("\n\n")
	if (fix && next !== source) writeFileSync(path, next)
}

for (const relative of [
	"src/components/routes/system/automatic-updates.tsx",
	"src/components/systems-table/power-actions.tsx",
	"src/components/add-system.tsx",
]) {
	const component = readFileSync(join(root, relative), "utf8")
	if (/ >\s*[A-Z][A-Za-z ]+\s*</.test(component)) {
		console.error(`Visible hardcoded string found in ${relative}`)
		failed = true
	}
}
if (failed) process.exit(1)
