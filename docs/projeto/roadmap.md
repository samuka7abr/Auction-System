# Roadmap e escopo

[← índice](../README.md)

## Etapas

| Etapa | Entrega |
| --- | --- |
| **1** | Schema e migrations, `docker compose` com recursos fixados, API Gin, engine otimista com classificação de desfecho, suíte de conformidade, `cmd/seed`, `cmd/checker` |
| 2 | Engine pessimista, middleware de idempotência, duplicatas injetadas no k6, `bid_attempts_per_accept` migra do k6 para o servidor |
| 3 | Engine single-writer com shards e commit em lote, métricas de profundidade e de lote |
| 4 | Fechamento via Redis Streams, `closerd` materializando `status`, cenários de caos |
| 5 | Matriz de 36 células, sweep de pool do pessimista, dashboards Grafana |
| 5b | Tick agregado no WebSocket, geração de tipos, painel React de três colunas |
| 6 | Escrita dos resultados, gráfico de cruzamento, README final |

Cada etapa é quebrada em specs do tamanho de um PR, em `docs/specs/etapa-n/`:

| Spec | Entrega |
| --- | --- |
| [etapa-1/01-spec-fundacao.md](../specs/etapa-1/01-spec-fundacao.md) | Compose com recursos verificados, migration `001`, pool, `/readyz` com fail-fast de schema, `cmd/seed` |
| [etapa-1/02-spec-engine-otimista.md](../specs/etapa-1/02-spec-engine-otimista.md) | `BidEngine`, engine otimista em um statement, envelope de três formas, as três rotas, métricas de lance, suíte de conformidade |
| [etapa-1/03-spec-carga-e-checker.md](../specs/etapa-1/03-spec-carga-e-checker.md) | Harness k6 com o apostador agressivo e a política de retentativa, `cmd/checker` com os invariantes, a célula reprodutível que a etapa 5 repete 36 vezes |
| [etapa-2/01-spec-engine-pessimista.md](../specs/etapa-2/01-spec-engine-pessimista.md) | Engine pessimista em transação com `SELECT ... FOR UPDATE`, `lock_wait_duration_seconds` legível contra a confirmação, a segunda engine passando na suíte que já existe |
| [etapa-2/02-spec-idempotencia.md](../specs/etapa-2/02-spec-idempotencia.md) | Middleware de idempotência sobre Redis acima do switch de estratégia, `X-Idempotency-Key` como fio do lance lógico, `idempotency_hits_total` e `bid_attempts_per_accept` medido pelo servidor |
| [etapa-2/03-spec-duplicatas-e-invariantes.md](../specs/etapa-2/03-spec-duplicatas-e-invariantes.md) | Duplicatas injetadas no k6, replay contado sem duplicar aceite, I5 exato e I7 fechando a etapa 2 |
| [etapa-3/01-spec-engine-single-writer.md](../specs/etapa-3/01-spec-engine-single-writer.md) | Shards com propriedade exclusiva do leilão, decisão em memória, commit em lote num statement, `201` só depois do commit, conformidade sem alterar a suíte |
| [etapa-3/02-spec-metricas-do-shard.md](../specs/etapa-3/02-spec-metricas-do-shard.md) | As quatro séries do mecanismo single-writer, o custo da durabilidade medido por lance aceito em vez de subtraído, o tamanho do lote como distribuição e a profundidade do inbox lida no scrape |
| [etapa-4/01-spec-fechamento-e-closerd.md](../specs/etapa-4/01-spec-fechamento-e-closerd.md) | Varredor de vencidos publicando no Redis Stream, `closerd` materializando `status` e `closed_at` com as duas guardas que o impedem de fechar cedo ou duas vezes, a fila medida no produtor em duas séries, e I8 |
| [etapa-4/02-spec-caos.md](../specs/etapa-4/02-spec-caos.md) | Injetor de falhas por fora dos processos, os quatro cenários da tabela de caos sob carga real, `chaos.json` provando que a injeção aterrissou, e o verificador aprendendo a diferença entre célula quebrada de propósito e célula que não vale |
| [etapa-5/01-spec-matriz.md](../specs/etapa-5/01-spec-matriz.md) | As 36 células na ordem que mantém a comparação honesta, o `auctiond` recriado e provado a cada uma, a célula-controle com banda, e o agregador que recusa a matriz em vez de publicar linha duvidosa |

As decisões de design estão em [decisoes/etapa-1.md](../decisoes/etapa-1.md), [decisoes/etapa-2.md](../decisoes/etapa-2.md), [decisoes/etapa-3.md](../decisoes/etapa-3.md), [decisoes/etapa-4.md](../decisoes/etapa-4.md) e [decisoes/etapa-5.md](../decisoes/etapa-5.md). Várias delas foram tomadas cedo de propósito: o contrato de durabilidade e o envelope uniforme de resposta precisam existir antes da primeira engine, ou as etapas 2 e 3 quebrariam a API que a etapa 1 publicou.

---

## Fora de escopo

Registrado de propósito, porque saber o que não fazer é parte do desenho.

| Não tem | Por quê |
| --- | --- |
| Múltiplas linguagens | Escolher três linguagens para seis serviços adiciona manutenção, não capacidade. A comparação entre estratégias exige a mesma linguagem nas três, ou o benchmark não significa nada |
| API Gateway, Auth Service | Não mudam nenhuma curva do gráfico. Identidade é `X-User-Id` no header; quando virar JWT, é um middleware e nada abaixo dele muda |
| gRPC | Otimização de transporte para um problema que não é de transporte |
| Kubernetes, Terraform, EKS | Custo real e semanas de trabalho para não alterar nenhum resultado. Orquestração é assunto de outro projeto |
| SNS/SQS | Redis Streams entrega as mesmas garantias localmente e deixa o mecanismo de retentativa visível |
| Microserviços | Dois processos, e o segundo existe apenas porque precisa ser morto no teste de caos |
| Catálogo, carrinho, pagamento | Leilão aqui é carga de trabalho, não produto |

**Regra de escopo:** uma pergunta, três implementações, um benchmark. Toda ideia nova passa pelo filtro "isso muda o gráfico?". Se não muda, fica de fora.
