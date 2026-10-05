# Configuração de agentes

## Objetivo e estado

O usuário solicitou inicialmente copiar TOMLs e instruções Markdown de `/travel-ai`. A cópia foi feita, mas depois o usuário substituiu o harness para reduzir delegações, tarefas e verbosity.

A configuração vigente está em AGENTS.md, `.codex/config.toml` e `.codex/agents/worker.toml`: principal executa e integra o trabalho; workers Luna xhigh apenas para tarefas simples independentes. Os papéis antigos não fazem parte da configuração atual. README foi alinhado a essa orientação.

## Checks

TOMLs atuais parseados com `tomllib`; AGENTS.md e configurações fornecidas pelo usuário preservados. Entrega Git junto com a demanda de upgrades do sistema operacional.
