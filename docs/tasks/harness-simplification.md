# Simplificação do harness

- Requisitos v1 aprovados explicitamente pelo usuário em 2026-10-04; execução autorizada por “Pode seguir”.
- Principal por padrão: Sol `medium` ou seleção explícita da UI; verbosity `low`.
- Somente workers Luna `xhigh`, até dois, com tarefa simples, arquivos exclusivos, contratos resolvidos e trabalho independente para o principal.
- Remover tech lead/developer/QA, gates intermediários, registro obrigatório, polling e mensagens rotineiras.
- Manter revisão integrada, checks pertinentes, segurança e preservação de alterações alheias.
- Refinamento aprovado pelo principal: diretrizes compartilhadas em AGENTS.md, configuração principal mínima e um único papel worker sem delegação.
- Evidências: TOML válido; `config/read` do app-server com cwd do projeto confirmou Sol `medium`, verbosity `low` e limite 2. Overrides explícitos de sessão prevaleceram; campos do worker aceitos pelo Codex.
- Revisão do principal: único papel `worker`, delegação desativada nele; critérios de independência, escopo exclusivo, registro opcional e revisão integrada conferidos. Diff comparado com cópias anteriores em `/tmp/beszel-harness-before` (arquivos untracked sem histórico Git).
- Snapshot: HEAD `1b9f5989`, workspace dirty com alterações preexistentes; arquivos desta entrega untracked. SHA-256 abreviado: AGENTS.md `c9bec7ca3737`, config.toml `d3dcc182ff94`, worker.toml `4511b9ff760c`; registro desta demanda separado.
- Limitações: validação estrita completa bloqueada por `plugins.visualize@openai-bundled.approval_policy` na configuração global; leitura normal também avisa sobre `sandbox_mode` nesse plugin. Nenhuma configuração global alterada. UI e spawn real não exercitados; aceitação dos campos do worker verificada por overrides de configuração. Testes da aplicação não pertinentes.
- Aceite funcional do principal: critérios estáticos e carregamento normal aprovados; sem QA separado conforme fluxo v1 autorizado. Sem alegação de economia quantitativa de tokens.
- Estado: concluído. Próxima ação: abrir nova sessão para carregar as instruções e papéis atualizados.
