# Atualizações do sistema operacional fim a fim

## Objetivo e alcance

Beszel Plus deve atualizar índices APT, instalar os upgrades autorizados e verificar o resultado, automaticamente e por solicitação do administrador. A demanda nasceu de uma máquina com mais de 90 updates pendentes apesar de unattended-upgrades ativo.

Alcance aprovado pelo usuário nesta conversa: “Aprovo: todos os updates oficiais; terceiros só quando autorizados (recomendado).” Inclui máquinas gerenciadas com política legada de segurança; preservar políticas customizadas, monitoramento, desativação explícita e escolhas novas do administrador.

Entrega também autorizada: “Ao final Não esquece de dar o commit e fazer o PR para subir no GitHub e disparar o pipeline e versionamento”. Versão preparada: 0.3.0; publicar PR e snapshot, sem criar tag/merge automaticamente.

## Critérios verificáveis

- CA-01: ciclo completo refresh → upgrades permitidos → inventário/dpkg audit; comando iniciado ou timer ativo não é sucesso.
- CA-02: agenda local no Agent com OS_UPDATE_MANAGEMENT e política habilitados, mesmo sem Hub; monitoramento apenas permanece explícito.
- CA-03: oficiais da distribuição instalada, segurança e regulares; terceiros exigem autorização. Adoção legada uma vez, preservando off/monitor/custom, intervalos e reboot.
- CA-04: mais de 90 elegíveis instalados sem limite da lista visual; held/excluded/blocked têm motivos, dados ausentes continuam desconhecidos.
- CA-05: refresh parcial ou falho impede instalação; erro e etapa de instalação/verificação visíveis.
- CA-06: locks e falhas transitórias têm espera limitada/retry persistido; retomada e replay não duplicam instalações; revogação de política cancela retries antigos.
- CA-07: painel mostra etapa, última tentativa/sucesso, próxima execução/retry e contagens verificadas.
- CA-08: Agent unprivilegiado, helper tipado, manutenção serializada com APT/power/upgrade do Agent.
- CA-09: reboot opt-in, padrão false; sem troca de distribuição ou adição de repositórios.
- CA-10: testes de ciclo normal/zero/>90, refresh parcial/rede, políticas/migração, holds, locks, restart/replay, persistência e UI; instalador/documentação coerentes.
- CA-11: commit, push, PR e pipeline; versão consistente Hub/Agent/helper/UI/installers e seis assets Linux com hashes/metadados verificados.

## Implementação

- Helper protocol 3/capability update_cycle: refresh estrito, política efetiva, inventário Python apt fixo, unattended-upgrade e verificação final.
- Estado root separado: ID monotônico/watermark, histórico 128, etapas/resultado, revisão da política, datas e backoff 1/5/15/30/60 minutos. Persistência obrigatória antes de comandos.
- APT supervisionado sem kill por timeout IPC; retomada confere processo/locks/dpkg/candidatos. Unidade oneshot protege subprocessos.
- Política padrão official_all; Allowed-Origins/Origins-Pattern gerenciados e configuração efetiva conferida. Sem ampliar versões da distribuição.
- Agent agenda/retoma localmente, publica estado no snapshot; Hub correlaciona eventos por ciclo/tentativa/estado. UI v3; legado somente leitura com atualização necessária.
- Instalador preflight compatível com legado v2, pós-install v3 e binários pareados. CI valida PR/main/tag com token read; publicação somente tag após validação, token write isolado.

## Estado e checks

Implementação revisada e integrada pelo principal sob AGENTS.md/config.toml atuais, preservados como fornecidos pelo usuário. Corrigidos polling de progresso, respostas atrasadas, persistência concorrente, histórico grande, intervalo zero e recuperação de IPC anterior ao claim.

- `make test`: passou. Casos incluem 120 upgrades sem limite de instalação, inventário Python de 505 candidatos, zero upgrades, holds/exclusões, refresh parcial/rede/assinatura, backoff persistido, restart em todas as fases, filho vivo/revogação, dpkg audit, escrita falha e replay após rotação de 128 ciclos.
- Race do helper/entities e testes focados de Agent/Hub para ciclos: passaram. Check ampliado falhou em ConnectionManager, prioridade de GPU e SystemManager; não há alterações nesses caminhos de concorrência. Não alegar race global aprovado.
- Bun: 5 testes, 19 expectativas; traduções de updates: passaram. Build web e Biome dos três arquivos do recurso: passaram; Biome dos sete arquivos tocados: sem erros, 32 warnings legados de `any`. Biome global: 84 erros/42 warnings, sem aprovação global.
- Navegador Chromium: componente real, estilos reais e catálogo pt-BR; instalação, retry, pendências, inventário desconhecido, helper v2 somente leitura, pedido manual v3, snapshot atrasado, deduplicação e mobile 390px. APIs simuladas; capturas em `docs/tasks/assets/os-updates-end-to-end/`.
- Instaladores: `sh -n` e testes Go passaram. GoReleaser check, snapshot dos seis assets, SHA-256, ELF/arquitetura/componente e versão/commit embutidos: passaram. Hub/Agent/helper amd64 reportam o mesmo snapshot; Agent/helper reportam protocolo 3. TOMLs atuais válidos (2/2); `git diff --check` passou.

Checks inicialmente interrompidos por quota de /tmp e caminho de socket longo foram repetidos em condições válidas. Nenhum APT real, host de produção, reboot ou publicação de release foi executado. O rollout requer atualizar Agent e helper pareados nas máquinas e manter OS_UPDATE_MANAGEMENT habilitado.

Entrega GitHub: commit funcional `7479c093`, branch `codex/os-updates-end-to-end` enviada e [PR #1](https://github.com/guzassis/beszel-plus/pull/1) aberto. O PR dispara validação/snapshot; acompanhar o resultado em [checks](https://github.com/guzassis/beszel-plus/pull/1/checks). Release estável 0.3.0 depende de merge/tag posteriores.
