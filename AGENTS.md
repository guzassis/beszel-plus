# Repository Guidelines

## Workflow

- Principal sozinho por padrão: planeje, implemente, revise o diff e valide os critérios. Mantenha decisões complexas e integração no principal; sem papéis ou gates intermediários.
- Somente o principal pode criar workers, até dois simultâneos, para tarefas simples e delimitadas. Delegue apenas com arquivos exclusivos, contratos estabilizados, dependências resolvidas e trabalho útil independente para o principal continuar. O ganho deve superar o custo de coordenação; tarefas sequenciais ficam no principal.
- Use o papel `worker` (Luna `xhigh`), `fork_turns="none"` e envie apenas objetivo, arquivos permitidos, critérios/checks e referências necessárias. Workers não delegam. Preserve alterações alheias; não edite arquivos atribuídos a um worker ativo.
- Uma atribuição e uma entrega por worker; mensagens adicionais apenas para bloqueios ou correções necessárias. Reutilize threads quando útil, sem ultrapassar o limite.
- Evite polling e consultas repetidas de status. Continue trabalho independente; aguarde a entrega com intervalos longos apenas quando necessário. Não refaça a exploração já entregue; revise o diff e valide a integração.
- Leia somente arquivos relevantes e a demanda ativa, quando existir. Use `rg`, agrupe consultas independentes e limite logs. Consulte versões/diffs pelo Git; arquivos untracked não têm histórico presumido.
- Responda com o mínimo necessário: atualizações breves, sem narrar passos rotineiros; ao concluir, informe resultado, checks e limitações. Pergunte apenas quando faltar decisão que afete o trabalho; sem aprovações recorrentes para ações já autorizadas.
- Use `docs/tasks/<id>.md` apenas para trabalhos longos, retomáveis ou quando solicitado: objetivo, critérios, estado e checks. Sem cerimônia de versionamento ou aprovação obrigatória.
- O principal revisa todas as alterações e verifica o resultado integrado. Checks bloqueados ou ausentes não passaram; mudanças posteriores exigem repetir os checks afetados. Encerre ao cumprir os critérios.
- Toda entrega via PR deve incluir uma nova versão e o envio da tag `vX.Y.Z`, inclusive mudanças de testes ou documentação. Incremente `+0.0.1` para ajustes, correções e entregas menores; `+0.1.0` para funcionalidades e entregas maiores. Alinhe as versões dos componentes e instaladores, valide os checks e publique a tag após integrar o PR.

## Project & Commands

- Go: Hub em `internal/cmd/hub`, Agent em `internal/cmd/agent`, helper em `internal/cmd/maintenance-helper`; serviços em `internal/`. React/Vite em `internal/site/src`; instaladores em `agent/`, guias e exemplos em `supplemental/`.
- Build: `make build`, `make build-agent`, `make build-hub`; `SKIP_WEB=true make build-hub` reutiliza assets existentes.
- Dev: `make dev-server`, `make dev-hub`, `make dev-agent`; frontend aceita Bun ou npm.
- Go: `make test` usa a tag `testing`; lint com `make lint`.
- Frontend: `npm run check --prefix internal/site`; formatação com `npm run format --prefix internal/site`.

## Quality & Safety

- Go com `gofmt`, nomes exportados convencionais e pacotes focados; testes ao lado do código em `*_test.go`, funções `TestFeature`. TypeScript/React com Biome e padrões locais; strings de UI seguem Lingui.
- Para mudanças de comportamento, adicione/ajuste testes relevantes e execute `make test`. Para frontend, execute Biome e verifique a tela afetada. Mudanças de instaladores/releases exigem checks dos scripts e documentação pertinentes. Não repita checks sem mudança ou preocupação nova.
- Nunca inclua credenciais ou configuração específica da máquina. Preserve os limites de privilégio entre Agent e helper; mudanças de permissões exigem atualizar documentação operacional ou de segurança.
- Commits com Conventional Commits. PRs seguem `.github/pull_request_template.md`: descrição, changelog pertinente, documentação e screenshots para mudanças visuais.
