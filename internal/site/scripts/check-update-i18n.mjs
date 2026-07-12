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
}

const updateReference = /automatic-updates\.tsx|alerts-history-columns\.tsx|src\/lib\/alerts\.ts/
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
		if (!updateReference.test(block)) return block
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

const component = readFileSync(join(root, "src/components/routes/system/automatic-updates.tsx"), "utf8")
if (/ >\s*[A-Z][A-Za-z ]+\s*</.test(component)) {
	console.error("Visible hardcoded string found in automatic-updates.tsx")
	failed = true
}
if (failed) process.exit(1)
