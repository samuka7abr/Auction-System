# Sistema de Leilão sob Alta Contenção

> Três estratégias de concorrência para o mesmo problema, medidas lado a lado.
> Go, PostgreSQL e Redis. Um binário, um worker, um harness de carga.

---

## O problema

Leilão tem uma propriedade que quase nenhum sistema CRUD tem: **contenção extrema sobre um único registro**.

Mil pessoas dando lance no mesmo item no último segundo é o pior caso possível de concorrência. Todas as escritas disputam a mesma linha, todas precisam ler o estado mais recente antes de decidir, e nenhuma pode ser perdida nem aplicada duas vezes.

A pergunta deste projeto:

> **Qual estratégia de concorrência sustenta throughput e latência quando N clientes disputam o mesmo leilão, e onde exatamente cada uma quebra?**

Não é um projeto sobre leilão. É um projeto sobre contenção, com leilão como carga de trabalho.

---

## Hipótese

O controle otimista com versionamento é a resposta que aparece em todo tutorial, e é a **pior** opção sob alta contenção: cada lance perdedor vira um `409`, o cliente retenta, a retentativa colide de novo, e o throughput desaba enquanto o p99 explode.

A hipótese a ser testada:

| Estratégia | Mecanismo | Custo por lance | Hipótese |
| --- | --- | --- | --- |
| **Otimista** | `UPDATE ... WHERE version = $n`, 409 + retry no cliente | 1 round-trip, mais N retentativas | Ganha com contenção baixa e muitos leilões paralelos; colapsa com contenção alta |
| **Pessimista** | `SELECT ... FOR UPDATE` dentro da transação | 1 round-trip, mais espera de lock | Estável, mas segura conexão do pool e limita a concorrência ao tamanho do pool |
| **Single-writer** | Uma goroutine dona exclusiva de cada leilão, lances entram por canal | 1 envio em canal, mais 1 commit em lote | Ganha com contenção alta: sem conflito, sem retentativa, sem lock |

A terceira é a aposta do projeto. Se cada leilão tem exatamente um escritor, a serialização é estrutural: não existe corrida para resolver, porque não existe corrida. O banco deixa de ser o ponto de sincronização e vira apenas durabilidade.

O entregável não é "implementei um leilão". É **o gráfico do cruzamento das três curvas conforme a contenção sobe**.

---

## A regra que mantém o projeto honesto

Três estratégias só são comparáveis se tudo o mais for idêntico. Quatro invariantes de método sustentam isso, e cada um deles existe porque a alternativa produziria um gráfico bonito e falso:

| Invariante de método | Se for violado |
| --- | --- |
| `201` significa **durável** nas três engines | O shard ganha por ter prometido menos, não por ser melhor |
| O cliente se comporta **igual** nas três | As engines recebem cargas diferentes e `aceitos/s` deixa de comparar |
| Pool, CPU e memória **fixos e iguais** nas três | Você mede a sua configuração e chama de resultado |
| Cada célula da matriz parte do **mesmo estado** | A última estratégia da lista parece a pior por efeito de ordem |

Está detalhado em [decisoes/etapa-1.md](decisoes/etapa-1.md).

---

## Mapa dos documentos

```
docs/
├── projeto/     o sistema: como é e por quê. Muda devagar
├── decisoes/    o registro do porquê, uma por etapa. Só cresce
└── specs/       o que construir a seguir, em unidades de PR. Some quando entrega
```

**`projeto/` — o desenho**

| Documento | Assunto |
| --- | --- |
| [projeto/arquitetura.md](projeto/arquitetura.md) | Componentes, processos, topologia, estrutura de pastas |
| [projeto/estrategias.md](projeto/estrategias.md) | A interface `BidEngine` e as três implementações |
| [projeto/api.md](projeto/api.md) | Contrato HTTP (Gin), envelope uniforme de resposta, códigos |
| [projeto/schema.md](projeto/schema.md) | Tabelas, migrations, invariantes garantidos pelo banco |
| [projeto/provas.md](projeto/provas.md) | Suíte de conformidade, verificador de invariantes, duplicatas, caos |
| [projeto/benchmark.md](projeto/benchmark.md) | k6, modelo do apostador, matriz, isolamento entre células |
| [projeto/observabilidade.md](projeto/observabilidade.md) | Métricas por etapa, Grafana |
| [projeto/painel.md](projeto/painel.md) | Painel React de três colunas |
| [projeto/roadmap.md](projeto/roadmap.md) | Etapas e fora de escopo |

**`decisoes/` — o porquê**

| Documento | Assunto |
| --- | --- |
| [decisoes/etapa-1.md](decisoes/etapa-1.md) | As 24 decisões de design da etapa 1, cada uma com a alternativa descartada. As de 19 a 24 emendam `estrategias.md` e `api.md` |
| [decisoes/etapa-2.md](decisoes/etapa-2.md) | As decisões da etapa 2, numeradas de 25 a 47. A 27 emenda a 22 para o caso transacional; as de 31 a 47 fecham idempotência, carga duplicada e os invariantes correspondentes |
| [decisoes/etapa-3.md](decisoes/etapa-3.md) | As decisões da etapa 3, a partir da 48. A 48 e a 52 emendam `estrategias.md` sobre o lote do shard; a 58 emenda a 16 por uma spec; as de 59 a 67 fecham a instrumentação do mecanismo, e a 59, a 60 e a 67 emendam `observabilidade.md` e a spec 01 |
| [decisoes/etapa-4.md](decisoes/etapa-4.md) | As decisões da etapa 4, a partir da 68. A 69 emenda o diagrama de `arquitetura.md` sobre quem publica o evento de fechamento, e a 73 emenda `observabilidade.md` desdobrando a série de fila em duas, publicadas pelo produtor |

**`specs/` — o que construir**

| Spec | Entrega |
| --- | --- |
| [specs/etapa-1/01-spec-fundacao.md](specs/etapa-1/01-spec-fundacao.md) | Compose, migration `001`, pool, `/readyz` com fail-fast de schema, `cmd/seed` |
| [specs/etapa-1/02-spec-engine-otimista.md](specs/etapa-1/02-spec-engine-otimista.md) | `BidEngine`, engine otimista, envelope, as três rotas, métricas de lance, suíte de conformidade |
| [specs/etapa-1/03-spec-carga-e-checker.md](specs/etapa-1/03-spec-carga-e-checker.md) | Harness k6 com o apostador agressivo, `cmd/checker`, a célula de benchmark reprodutível |
| [specs/etapa-2/01-spec-engine-pessimista.md](specs/etapa-2/01-spec-engine-pessimista.md) | Engine pessimista com `SELECT ... FOR UPDATE`, `lock_wait_duration_seconds`, a segunda curva do gráfico |
| [specs/etapa-2/02-spec-idempotencia.md](specs/etapa-2/02-spec-idempotencia.md) | Middleware de idempotência sobre Redis, `X-Idempotency-Key`, `idempotency_hits_total`, `bid_attempts_per_accept` saindo do k6 para o servidor |
| [specs/etapa-2/03-spec-duplicatas-e-invariantes.md](specs/etapa-2/03-spec-duplicatas-e-invariantes.md) | Duplicatas concorrentes e replay no k6, I5 exato e I7 provando chaves presentes e únicas |
| [specs/etapa-3/01-spec-engine-single-writer.md](specs/etapa-3/01-spec-engine-single-writer.md) | Engine single-writer com shards, decisão em memória e commit em lote, passando na suíte que já existe |
| [specs/etapa-3/02-spec-metricas-do-shard.md](specs/etapa-3/02-spec-metricas-do-shard.md) | `bid_accept_duration_seconds`, `journal_lag_seconds`, `shard_batch_size` e `shard_inbox_depth`: o custo da durabilidade medido direto, sem subtrair série de série |
| [specs/etapa-4/01-spec-fechamento-e-closerd.md](specs/etapa-4/01-spec-fechamento-e-closerd.md) | Varredor de vencidos no `auctiond`, `internal/stream` sobre Redis Streams, `closerd` materializando `status` e `closed_at` sem poder violar invariante, e I8 |

Modelo em [specs/spec-model.md](specs/spec-model.md).

---

## Como rodar

```bash
make up                              # postgres, redis, prometheus, grafana
make migrate                         # aplica migrations (golang-migrate)
make seed AUCTIONS=1                 # semeia N leilões
make run STRATEGY=optimistic         # sobe auctiond (e closerd, a partir da etapa 4)
make test                            # suíte de conformidade (testcontainers)
make bench                           # matriz completa, gera bench/results/
make check RUN=<id>                  # verificador de invariantes
make chaos                           # injeção de falhas sob carga
make types                           # regenera web/src/types.gen.ts das structs Go
make web                             # painel em http://localhost:5173
```

---

## Regra de escopo

Uma pergunta, três implementações, um benchmark. Toda ideia nova passa pelo filtro **"isso muda o gráfico?"**. Se não muda, fica de fora. O que ficou de fora, e por quê, está em [projeto/roadmap.md](projeto/roadmap.md).
