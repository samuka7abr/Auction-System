# Decisões — Etapa 4

[← índice](../README.md)

Registro das decisões tomadas ao desenhar as specs da etapa 4. A numeração continua a da [etapa 1](etapa-1.md), da [etapa 2](etapa-2.md) e da [etapa 3](etapa-3.md): as decisões de 1 a 67 continuam valendo, e as que são emendadas aqui dizem em qual ponto.

---

## Decisões da spec 01

Tomadas ao desenhar a [spec 01 da etapa 4](../specs/etapa-4/01-spec-fechamento-e-closerd.md), o fechamento via Redis Streams. Uma delas **emenda** o diagrama de [arquitetura.md](../projeto/arquitetura.md) e outra emenda a tabela de [observabilidade.md](../projeto/observabilidade.md). Onde houver divergência, vale o que está aqui.

---

### 68. O `closerd` materializa, e o `WHERE` reafirma o tempo do banco

O fechamento é um statement só, guardado duas vezes:

```sql
UPDATE auctions
   SET status = 'closed', closed_at = clock_timestamp()
 WHERE id = $1 AND status = 'open' AND ends_at <= clock_timestamp()
RETURNING closed_at, extract(epoch from (closed_at - ends_at))
```

**Por quê:** a decisão 12 fixou que fechamento é propriedade do tempo e não evento, e a consequência dela é que o `closerd` responde por performance, nunca por corretude. Um worker que fechasse por confiar na mensagem seria a violação exata dessa promessa: bastaria um replay, um id fabricado ou um relógio errado no produtor para carimbar `closed_at` num leilão vivo — e um leilão fechado cedo demais **recusa lance legítimo**, que é a única direção de erro pior que aceitar lance atrasado.

As duas guardas fazem trabalhos diferentes e nenhuma das duas é decorativa. `status = 'open'` é o que torna a materialização idempotente: a segunda entrega da mesma mensagem afeta zero linhas, e `closed_at` não se mexe. `ends_at <= clock_timestamp()` é o que torna o `closerd` incapaz de fechar cedo, seja qual for o conteúdo da mensagem. Juntas, elas permitem que o cenário de caos mate o processo em qualquer ponto sem que a suíte de invariantes tenha o que reclamar.

**A mensagem carrega um id e nada mais.** Nem `ends_at`, nem o topo, nem o vencedor: tudo é relido do banco, que é a autoridade. Uma mensagem que carregasse estado seria uma segunda fonte de verdade viajando por um transporte com entrega *at-least-once* — e a primeira vez que as duas discordassem, o `closerd` estaria escolhendo entre elas sem ter como saber qual está certa.

**O vencedor não é calculado aqui.** `highest_bid_cents` e `highest_bidder` já são escritos pelas três engines no mesmo statement que aceita o lance. O `closerd` carimba o fim, não apura o resultado — e é por isso que ele não precisa de transação, de lock nem de leitura prévia.

---

### 69. O evento nasce de um varredor no `auctiond`, não da engine e não do `closerd`

Emenda o diagrama de [arquitetura.md](../projeto/arquitetura.md), que desenha `STRAT -->|evento| REDIS` — a estratégia publicando o evento de fechamento.

Quem publica é um varredor: uma goroutine do `auctiond` que, a cada segundo, pergunta ao banco quais leilões estão abertos e vencidos e publica um `XADD` por leilão.

**Por que não a engine:** três motivos, e cada um sozinho já bastaria.

| Se a engine publicasse | O que quebraria |
| --- | --- |
| `XADD` no caminho quente | `bid_confirm_duration_seconds` passaria a medir Redis na borda do `ends_at` — que é o cenário inteiro do projeto |
| O evento só nasceria de um lance recusado | Um leilão que vence **sem ninguém tentar lance** nunca fecharia, e a matriz da etapa 5 tem células com 1000 leilões e VUs concentrados em poucos |
| Uma publicação por rejeição | Sob 1000 VUs num leilão vencido, seriam milhares de mensagens por segundo do mesmo fato |

**Por que não o `closerd`:** se o mesmo processo varresse e fechasse, o stream seria uma fila interna dele — e o cenário de caos perderia o sentido. O que se quer provar é que o fato *já publicado* sobrevive à morte de quem o consome, e isso exige que produtor e consumidor sejam processos diferentes. É a mesma razão pela qual o `cmd/checker` nunca pergunta ao Prometheus (decisão 4): um componente que confere a si mesmo não é evidência.

**O custo, dito em voz alta:** o `auctiond` passa a pagar uma consulta por segundo que não pagava nas etapas 1 a 3. Ela é indexável, roda fora do caminho quente e devolve zero linha em toda célula de benchmark (`ENDS_IN` é folgado o bastante para que nada feche no meio). Ainda assim é custo novo, e ele é pago **igualmente pelas três engines** — a matriz da etapa 5 roda inteira depois desta etapa.

---

### 70. Reemissão com supressão em memória, em vez de outbox transacional

O varredor reemite o evento enquanto o leilão continuar aberto e vencido, e suprime a reemissão do mesmo id por 30 segundos usando um mapa em memória, podado por idade.

**Por quê:** a alternativa correta de livro é um outbox: uma tabela onde o fato do fechamento é gravado na mesma transação que o produz, e um relay que publica dali e marca como publicado. Ela resolve o problema de "publiquei e o processo morreu antes do commit" — que aqui **não existe**, porque não há transação produzindo o fato. O fato é `ends_at <= now()`, que o banco recalcula a cada varredura de graça. Uma tabela nova, uma migration nova e um estado a mais para reconciliar, para dar durabilidade a um fato que já é derivável, seria complexidade paga em algo que a própria consulta já garante.

A reemissão faz o sistema se curar de todas as perdas possíveis sem código de recuperação nenhum: mensagem perdida por trim, mensagem que nunca chegou porque o Redis reiniciou, grupo recriado do zero. Enquanto a coluna `status` não mudar, o fato continua sendo publicado.

**Por que a supressão:** sem ela, um `closerd` parado por dez minutos acumularia 600 cópias por leilão vencido. As cópias são inofensivas (decisão 68), mas inflam a fila e transformam a leitura de atraso em ruído. Trinta segundos é folgado contra a latência normal de fechamento — que é milissegundos — e curto o bastante para que a recuperação de uma perda continue rápida.

**Limite registrado:** o mapa é do processo, e reiniciar o `auctiond` reemite tudo o que ainda estiver vencido e aberto. É o comportamento desejado e não a exceção: reiniciar é exatamente quando não se sabe mais o que foi publicado.

---

### 71. A etapa 4 não tem migration, e `ExpectedSchemaVersion` continua 1

Nenhuma tabela nova, nenhuma coluna nova, nenhum índice novo.

**Por quê:** a decisão de fazer `status` e `closed_at` nascerem na `001` (schema.md) foi tomada três etapas antes exatamente para que este PR não precisasse tocar o schema. Cobrar essa promessa é o barato aqui.

**Por que nem o índice parcial:** a tentação é `CREATE INDEX ... ON auctions (ends_at) WHERE status = 'open'`, para a consulta do varredor. A tabela tem entre 1 e 1000 linhas em toda célula deste projeto, e uma varredura sequencial nesse tamanho custa microssegundos — o índice não pagaria nem o `VACUUM ANALYZE` que o reset de célula já roda. E ele custaria caro em outro lugar: subir `ExpectedSchemaVersion` para 2 faz **todo binário anterior a este PR responder `/readyz` vermelho**, e a etapa 5 ainda vai querer rodar células de comparação com o que existe hoje.

**Limite registrado:** num sistema com milhões de leilões, o varredor sem índice é uma varredura sequencial por segundo, e a correção é uma linha de migration. Fica como não feito porque não muda nenhuma curva do gráfico.

---

### 72. O grupo é criado pelo produtor, no boot, e o stream é capado

`XGROUP CREATE auctions.expired closerd $ MKSTREAM` no boot do `auctiond`, ignorando `BUSYGROUP`. Todo `XADD` vai com `MAXLEN ~ 100000`.

**Por que o produtor cria:** se a criação fosse do `closerd`, o `auctiond` publicaria em um stream sem grupo enquanto o worker não tivesse subido — e o `XINFO GROUPS` do produtor, que é de onde saem as duas séries de fila (decisão 73), não teria o que reportar. Pior: as mensagens publicadas antes do grupo existir ficariam invisíveis para ele, porque um grupo criado em `$` só enxerga o que chega depois. Criando no boot do produtor, o grupo existe antes da primeira publicação, e um `closerd` que sobe meia hora depois encontra tudo.

**Por que `$` e não `0`:** as mensagens que interessam são as que descrevem um fato que ainda vale. Um grupo criado em `0` reprocessaria o histórico inteiro a cada recriação — e o resultado seria uma enxurrada de `already_closed`, que é trabalho sem informação. Se um fato ainda for verdade, a reemissão da decisão 70 o traz de volta em um segundo.

**Por que o teto:** o Redis do compose tem 512M e divide essa memória com as chaves de idempotência, que estão no caminho quente das três engines. Um stream sem teto é uma fila de memória escondida — o mesmo erro que o inbox limitado da decisão 56 evita dentro do processo. Cem mil entradas de dezenas de bytes são folga confortável, e o `~` deixa o Redis podar no limite de macro-nó, que é o barato.

**Consequência aceita:** o trim pode apagar uma entrada nunca consumida. A reemissão a traz de volta, e é essa combinação — teto no transporte, cura no produtor — que substitui a durabilidade que um broker de verdade daria.

---

### 73. A fila é medida no produtor, e são duas séries e não uma

Emenda [observabilidade.md](../projeto/observabilidade.md), que declara uma série só: `stream_pending_entries`, *"mensagens não confirmadas no fechamento"*.

Passam a existir duas, as duas publicadas pelo **`auctiond`**, lidas de um `XINFO GROUPS` no scrape:

| Série | O que conta | Quando cresce |
| --- | --- | --- |
| `stream_pending_entries` | entregues ao grupo e não confirmadas | o `closerd` está vivo e travado, ou morreu **com mensagem na mão** |
| `stream_backlog_entries` | publicadas e nunca entregues ao grupo | o `closerd` não está lendo — está parado, morto ou não subiu |

**Por que duas:** a série única mede a coisa errada no cenário que ela existe para explicar. Um `closerd` que foi morto e não voltou não produz pendência — ele produz **atraso**: as mensagens continuam sendo publicadas e ninguém as entrega a consumidor nenhum, então `pending` fica parado no punhado que estava em voo na hora da morte, enquanto a fila real cresce sem aparecer. Um painel com só `stream_pending_entries` mostraria uma linha quase plana durante a falha inteira.

**Por que no produtor:** porque a série que mede o consumidor não pode morar dentro dele. Se `stream_pending_entries` fosse publicada pelo `closerd`, o alvo pararia de ser raspado exatamente quando o número começasse a interessar, e o gráfico do cenário de caos teria um buraco no lugar da evidência. O `auctiond` está vivo durante os quatro cenários de falha, e a chamada custa um round-trip a cada 5 segundos, fora do caminho quente.

**Erro no scrape não vira zero.** Se o Redis não responder, ou se o Redis não souber calcular o `lag` — ele reporta nulo depois de certos trims —, a série **não é emitida** naquela raspagem. É a mesma regra da decisão 59: zero é uma afirmação diferente de silêncio, e publicar zero de fila enquanto o Redis está fora do ar seria a métrica mentindo justamente no incidente.

---

### 74. O `closerd` tem registry, `/metrics` e `/healthz` próprios, e não importa `internal/httpapi`

Segundo processo, segunda porta (`:8081`), segundo job de scrape no Prometheus.

**Por quê:** `internal/httpapi` é o contrato de lance — roteador do Gin, envelope uniforme, identidade, middleware de idempotência. Nada disso existe no worker, e importá-lo para reaproveitar `/healthz` traria Gin, o `Deps` inteiro e a obrigação de satisfazer campos que não têm sentido aqui. Um `http.ServeMux` com três rotas é menos código do que o adaptador seria.

**Por que um registry próprio e não o do `auctiond`:** são dois processos. Não há registry a compartilhar, e é justamente isso que dá a propriedade que o cenário de caos precisa: matar o `closerd` derruba um alvo de scrape e deixa o outro de pé, e o Prometheus registra a queda como `up{job="closerd"} 0` — que é um dado, não um buraco.

---

### 75. O desfecho do fechamento é classificado, e o round-trip extra é só no caminho frio

`auction_closings_total{result}` tem quatro valores: `applied`, `already_closed`, `gone` e `early`. O `UPDATE` que não afeta linha nenhuma dispara um `SELECT` de classificação; o que afeta não paga nada.

**Por quê:** é a decisão 1 aplicada de novo, no outro extremo do sistema. `RowsAffected() == 0` significa três coisas incompatíveis — o leilão já estava fechado, o leilão não existe mais, o leilão ainda não venceu — e cada uma diz algo diferente sobre o sistema:

| Resultado | O que ele denuncia |
| --- | --- |
| `already_closed` | entrega duplicada, que é o comportamento normal de um transporte *at-least-once*. É a prova de que a guarda da decisão 68 está funcionando |
| `gone` | o leilão sumiu entre a publicação e o consumo — o `TRUNCATE` do reset entre células (decisão 13), e nada mais neste projeto |
| `early` | mensagem descrevendo um fato que não é verdade. Deveria ser impossível, já que produtor e consumidor perguntam as horas ao mesmo banco; se aparecer, é achado sobre o sistema e não sobre o leilão |

Somar as três num `noop` faria o reset entre células e um relógio quebrado contarem no mesmo número. O custo é um `SELECT` num caminho que, numa célula saudável, tem zero ocorrência.

**Todos os quatro confirmam a mensagem.** Nenhum deles é motivo para deixar a entrada na pending list: os três desfechos frios descrevem fatos terminais, e reciclá-los seria pedir ao Redis que reentregasse para sempre uma mensagem que não tem mais nada a fazer. Só erro de infraestrutura — o banco não respondeu — deixa a entrada sem `XACK`.

---

### 76. `close_lag_seconds` é calculado pelo banco, no `RETURNING`

A latência de materialização sai de `extract(epoch from (closed_at - ends_at))`, computado pelo Postgres no mesmo statement que fecha.

**Por quê:** é a decisão 22 outra vez, e a 62 pelo avesso. As duas pontas do intervalo são colunas escritas pelo relógio do banco; subtrair delas um `time.Now()` do contêiner do `closerd` mediria o fechamento somado ao offset entre dois relógios, e faria isso em silêncio, num compose onde o erro é pequeno o bastante para nunca chamar atenção. O `RETURNING` já está lá para devolver `closed_at`; a subtração vai junto e não custa round-trip nenhum.

**O que a série mede, dito com precisão:** o intervalo entre o instante em que o leilão devia estar fechado e o instante em que a coluna passou a dizer isso. Ela inclui o meio segundo médio de espera pelo tique do varredor, a viagem pelo Redis e a fila — de propósito. É a pergunta do operador, e é ela que o cenário de caos vai medir voltando ao normal depois que o worker ressuscita.

**Buckets próprios, e nenhuma tentativa de alinhá-los ao `confirm`.** A decisão 26 exige fronteiras compartilhadas entre séries que se leem **uma contra a outra**, e esta não se lê contra nenhuma: `bid_confirm_duration_seconds` vive em milissegundos e mede o caminho quente; esta vive em segundos e mede um worker que nem tenta ser rápido. Compartilhar fronteiras aqui só empilharia tudo no `+Inf` de um lado e no primeiro bucket do outro.

---

### 77. Uma mensagem por vez: o `closerd` não fecha em lote

Cada entrada lida vira um `UPDATE` próprio, processado em sequência, sem agrupamento.

**Por quê:** o lote da decisão 53 existe porque o shard aceita centenas de lances por segundo e o `fsync` é o gargalo dele. Aqui a carga é outra: no pior cenário deste projeto, mil leilões vencem juntos no fim de uma célula — mil `UPDATE`s ao longo de alguns segundos, num processo que não está no caminho de ninguém. Agrupar traria de volta toda a semântica do lote que aborta (decisão 53): um id ruim derrubaria o `UPDATE` dos outros, e seria preciso decidir a quem `XACK`ar depois de uma falha parcial.

O laço sequencial tem uma propriedade que o lote não tem, e ela é a que o cenário de caos usa: em qualquer instante existe **no máximo uma** mensagem em voo, então matar o processo deixa no máximo uma entrada na pending list, e o que o `XAUTOCLAIM` recupera é exatamente ela.

---

### 78. `XAUTOCLAIM` roda no mesmo laço, antes de cada leitura, e a DLQ é só para mensagem malformada

O laço do consumidor é: `XAUTOCLAIM` com `min-idle` de 30 segundos, depois `XREADGROUP` com `BLOCK` de 5 segundos. Uma mensagem cujo payload não é um uuid vai para `auctions.expired.dead` e recebe `XACK`.

**Por que no mesmo laço:** uma goroutine separada de reivindicação precisaria coordenar com a leitura para não processar a mesma entrada duas vezes, e a coordenação seria um mutex protegendo o que uma goroutine só resolve por construção — o mesmo argumento da decisão 55, no outro processo. Um consumidor, um laço, e a recuperação é a primeira coisa que ele faz.

**Por que 30 segundos de ociosidade:** o `min-idle` é o que separa "o worker está demorando" de "o worker morreu com a mensagem na mão". Um fechamento normal leva milissegundos; 30 segundos é ordens de grandeza acima disso, e ainda assim curto o bastante para que o cenário de caos observe a reivindicação acontecer sem esperar por um minuto.

**Por que a DLQ não é por contagem de entregas:** o desenho clássico manda mandar para a DLQ na n-ésima entrega. Ele resolve o *poison message* — a mensagem que sempre falha e recicla para sempre. Neste sistema, as únicas duas formas de falha permanente são um payload malformado, que é detectável na primeira leitura e vai direto para a DLQ, e um leilão que sumiu, que é desfecho terminal e recebe `XACK` (decisão 75). Todo o resto é o banco não respondendo — e nesse caso reciclar é precisamente o comportamento certo.

**Limite registrado:** se um dia existir uma mensagem que falha para sempre com erro transitório, ela recicla para sempre. O teto por contagem exigiria `XPENDING` com `RetryCount` a cada volta do laço, e fica como não feito por resolver um caso que ainda não pode acontecer.

---

### 79. Não existe invariante "todo leilão vencido está fechado"

O `cmd/checker` ganha **I8**, que prova coerência do fechamento, e não prova convergência.

```text
I8  fechamento coerente:  (status = 'closed') = (closed_at IS NOT NULL)
                          e nunca closed_at < ends_at
```

**Por quê:** o invariante que a gente quer escrever — todo leilão com `ends_at` no passado tem `status = 'closed'` — é **falso enquanto o `closerd` está morto**, e matar o `closerd` é um cenário de teste desta mesma etapa. Um verificador que reprova a célula por causa da falha que a célula está injetando de propósito não é verificador, é ruído: a primeira coisa que se faz com ele é desligá-lo no cenário de caos, e a partir daí ele não prova mais nada em lugar nenhum.

O que I8 prova vale sempre, com worker vivo ou morto: as duas colunas nunca discordam entre si, e nenhum leilão foi fechado antes da hora. Ele é o par exato da guarda da decisão 68 — o que o `WHERE` torna impossível, o checker confere que continua impossível.

**A convergência é medida, não afirmada.** Depois que o worker volta, `auction_close_lag_seconds` mostra em quanto tempo a fila drenou e `stream_backlog_entries` volta a zero. Isso é um número num relatório, que é o lugar certo para uma propriedade que depende de o processo estar de pé.

**O que I8 dá de graça, junto com I4:** `created_at <= ends_at` (I4) e `ends_at <= closed_at` (I8) encadeiam em `created_at <= closed_at` — *nenhum lance entrou depois do leilão ter sido fechado* — sem uma terceira consulta. Escrevê-la seria confirmar as outras duas, que é o que I1 já registra não fazer com o `UNIQUE`.

---

### 80. O `closerd` sobe no compose e roda durante toda célula, e `env.sh` grava os limites dele

Serviço novo no `docker-compose.yaml`, com CPU e memória fixadas, sem `profiles`, e uma entrada nova em `bench/results/<run>/env.json`.

**Por quê:** o terceiro invariante de método diz que recurso é fixo e igual nas três estratégias, e a decisão que colocou o Redis sob limite na etapa 2 estabeleceu o precedente: quando um processo passa a compartilhar a máquina com o benchmark, ele entra no `env.json` ou vira variável escondida dentro de um número publicado.

**Por que sem `profiles`:** um `closerd` que só sobe em alguns runs faria duas células da mesma matriz medirem sistemas diferentes. Ele custa quase nada quando não há nada para fechar — fica bloqueado num `XREADGROUP` —, e a alternativa seria a etapa 5 ter de lembrar de ligá-lo.

**Meia CPU e 256M:** é um consumidor sequencial que faz um `UPDATE` por vez (decisão 77). O limite existe para que ele não possa competir com o `auctiond` num pico, não porque ele precise do teto.

**O que isso implica para a comparação, dito em voz alta:** células rodadas antes desta etapa e depois dela não são estritamente comparáveis — o `auctiond` ganhou uma consulta por segundo (decisão 69) e a máquina ganhou um processo. A matriz da etapa 5 roda inteira depois daqui, então as três estratégias pagam o mesmo custo, que é o que o invariante de método exige. Os números avulsos das etapas 1 a 3 continuam servindo para o que serviram: verificar mecanismo, nunca para entrar no gráfico final.
