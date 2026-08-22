# Decisões — Etapa 2

[← índice](../README.md)

Registro das decisões tomadas ao desenhar as specs da etapa 2. A numeração continua a da [etapa 1](etapa-1.md): as decisões de 1 a 24 continuam valendo, e as que são emendadas aqui dizem em qual ponto.

---

## Decisões da spec 01

Tomadas ao desenhar a [spec 01 da etapa 2](../specs/etapa-2/01-spec-engine-pessimista.md), a engine pessimista.

---

### 25. A pessimista paga a transação inteira, e nada além dela

`Begin` → `SELECT ... FOR UPDATE` → CTE de escrita → `Commit`. Quatro round-trips no aceite, três na rejeição, conexão presa do primeiro ao último.

**Por quê:** existe uma forma de escrever a pessimista em um statement só, com `FOR UPDATE` dentro de uma CTE, e ela funciona. Mas o que ela implementa não é mais pessimismo: o lock nasce e morre dentro do statement, a decisão volta para dentro do SQL, e o que sobra é a engine otimista com outro mecanismo de serialização. O projeto não pergunta *qual SQL é mais rápido*, pergunta **o que acontece quando um cliente segura um lock enquanto decide** — e é essa a forma que a etapa 5 vai medir.

A decisão 19 já fixou o outro lado: o otimista não ganha uma transação de graça, porque precisar de transação é um custo real do pessimismo. Esta decisão é a mesma regra vista do outro lado — o pessimismo paga o custo que é dele, e não paga nenhum que não seja.

**O que ficou de fora por não ser do mecanismo:** `UPDATE` e `INSERT` vão numa CTE só, como no otimista, em vez de dois round-trips separados. E o `UPDATE` não repete as guardas de `status` e `ends_at`: o lock já garante que o estado lido é o vigente, e repetir a guarda criaria um ramo "impossível" que ninguém saberia classificar se um dia fosse alcançado.

**Consequência que precisa ser encarada:** se a pessimista perder no gráfico, a primeira pergunta hostil será *"ela perdeu por mecanismo ou pelos quatro round-trips?"*. A resposta é a decisão 26, e ela é medida, não argumentada.

---

### 26. `lock_wait_duration_seconds` compartilha os buckets de `bid_confirm_duration_seconds`

Mesmos catorze buckets exponenciais, de 1ms a 8,192s.

**Por quê:** as duas séries só respondem à pergunta que interessa se puderem ser lidas uma contra a outra, bucket a bucket. Se sob alta contenção a espera em lock domina a confirmação, os quatro round-trips da decisão 25 são ruído dentro do número, e a crítica morre no gráfico — que é a mesma manobra da decisão 6 com `RETRY_POLICY`. Se **não** dominar, o projeto descobre isso antes de publicar, e não depois.

Buckets diferentes obrigariam a comparação a passar por interpolação de quantis, que é precisamente onde uma diferença de dez pontos percentuais se esconde.

**Limite registrado:** o histograma mede o `SELECT ... FOR UPDATE` inteiro — round-trip, planejamento e espera — e não a espera pura. Extrair a espera pura exigiria amostrar `pg_locks`, ou seja, custo no caminho quente para medir o caminho quente. Não é preciso: o piso do histograma na célula de 1000 leilões, onde ninguém disputa a mesma linha, **é** o custo do round-trip. A calibração vem da própria matriz.

---

### 27. Dentro de transação, `now()` é o instante do `BEGIN` — a pessimista lê `clock_timestamp()`

Emenda a decisão 22 para o caso transacional: a autoridade continua sendo o relógio do Postgres, mas a função muda.

**Por quê:** `now()` é `transaction_timestamp()`. No otimista isso é inofensivo, porque cada statement é a própria transação. Na pessimista, o `BEGIN` acontece **antes** da espera em lock: sob mil VUs, a transação pode ficar centenas de milissegundos na fila e ainda assim ler um `now()` anterior à espera. A guarda de fechamento ficaria mais frouxa que a do otimista, e a diferença apareceria exatamente na borda do `ends_at` — o cenário inteiro do projeto — parecendo diferença entre estratégias quando é diferença entre relógios.

`clock_timestamp()` é avaliado durante a execução do statement, depois da espera. A pessimista fica com a guarda pelo menos tão estrita quanto a do otimista.

**Efeito colateral bom:** `bids.created_at` continua no `DEFAULT now()`, ou seja, o instante do `BEGIN`, que é anterior ao `clock_timestamp()` que autorizou o aceite. Logo `created_at < ends_at` vale por construção sempre que o lance passa, e o invariante I4 do `cmd/checker` — *nenhum lance depois do fechamento* — não pode acusar falso positivo por causa do lock.

---

### 28. A pessimista instrumenta o próprio lock, e é a segunda exceção registrada da decisão 23

`lock_wait_duration_seconds` é observada de dentro da engine. `bid_confirm_duration_seconds` e `bid_outcomes_total` continuam vindo do decorator, nas três.

**Por quê:** o decorator mede a fronteira do `BidEngine`, e a espera em lock é um intervalo interno que ele não tem como enxergar. É a mesma situação já prevista para `bid_accept_duration_seconds` no shard da etapa 3, e a regra que a decisão 23 protege continua intacta: o que **compara** as três estratégias é medido num lugar só; o que descreve o mecanismo de uma delas é medido dentro dela.

O teste do limite é simples: nenhuma série instrumentada por dentro pode aparecer no eixo de comparação da etapa 5. `lock_wait` explica a pessimista; não julga o otimista.

**Sem label `strategy`:** só uma engine produz a série. Um label com um valor único sugeriria que as outras duas reportam zero, quando na verdade elas não reportam nada — e zero é uma afirmação diferente de silêncio.

---

### 29. Nenhuma retentativa por deadlock, porque a pessimista não pode entrar em deadlock

Sem laço de retry, sem tratamento de `40P01`.

**Por quê:** cada transação tranca **uma** linha de `auctions` e nunca uma segunda. O `INSERT` em `bids` toma `FOR KEY SHARE` sobre a mesma linha que a transação já detém em modo mais forte, na mesma transação. Sem duas linhas, não existe ordem de aquisição para inverter, e sem inversão não existe ciclo.

Um laço de retry aqui seria código que nunca executa — e pior: se um dia a engine passar a trancar duas linhas, o deadlock precisa aparecer **alto**, no log e na métrica, em vez de ser absorvido em silêncio por uma retentativa que faria o benchmark medir uma engine diferente da descrita.

**Corolário verificável:** sob a pessimista, `UNIQUE (auction_id, seq)` é matematicamente inalcançável, porque só o dono do lock calcula `seq`. Se essa restrição disparar alguma vez, não é contenção: é bug de engine, e a restrição permanece no schema como asserção viva.

---

### 30. `READ COMMITTED`, não `SERIALIZABLE`

A transação da pessimista roda no isolamento padrão.

**Por quê:** `SERIALIZABLE` transformaria toda disputa em `40001` mais retentativa. Isso não é a estratégia pessimista com mais segurança — é a **estratégia otimista com a retentativa movida para dentro do banco**, com amplificação de retry, colapso sob contenção e tudo o mais que a hipótese atribui ao otimista. Seria uma quarta célula no eixo de estratégias, respondendo a uma pergunta que este projeto não fez, e cujo lugar natural é o trabalho seguinte.

Em `READ COMMITTED`, `SELECT ... FOR UPDATE` já entrega o que a estratégia promete: quem detém o lock lê a última versão comitada e é o único que escreve até o `COMMIT`.

**Fica registrado como não medido:** o projeto não afirma nada sobre `SERIALIZABLE`. Afirmar sem medir seria exatamente o que os quatro invariantes de método existem para impedir.

---

## Decisões da spec 02

Tomadas ao desenhar a [spec 02 da etapa 2](../specs/etapa-2/02-spec-idempotencia.md), o middleware de idempotência. Três delas **emendam** o que já estava publicado em `projeto/provas.md` e `projeto/observabilidade.md`. Onde houver divergência, vale o que está aqui.

---

### 31. A chave de idempotência nomeia o lance lógico, não a requisição

Uma chave por lance lógico, mantida por todas as tentativas até o `201` ou até a desistência. Sem impressão digital do corpo.

**Por quê:** o projeto fez duas promessas em documentos diferentes sem checar se elas cabem juntas. [provas.md §3](../projeto/provas.md) quer que um reenvio idêntico devolva a resposta guardada — a chave nomeando **uma requisição**, como em qualquer API de pagamento. [observabilidade.md](../projeto/observabilidade.md), linha 31, quer que a chave seja o fio que correlaciona três requisições como tentativas do mesmo lance — a chave nomeando **um lance lógico**.

As duas leituras não convivem, porque o apostador da decisão 5 re-mira a cada rejeição: a tentativa 2 tem, por construção, um corpo diferente da tentativa 1. Sob a semântica de requisição sobram duas saídas, e as duas matam uma promessa: ou o cliente troca de chave a cada tentativa, e não existe fio nenhum, ou ele mantém a chave e a tentativa 2 volta como erro de reuso, deixando o apostador parado contra o próprio middleware.

**O preço, que não é pequeno:** um cliente que reusar uma chave para um lance genuinamente diferente recebe de volta a resposta do primeiro. Este desenho não tem como distinguir os dois casos, e fingir que tem seria pior do que registrar. O que substitui a impressão digital é o formato da chave (decisão 32), que torna a colisão acidental um evento que não acontece.

**A consequência que faz o desenho funcionar:** se o corpo não distingue duplicata de retentativa, o que distingue é o **tempo**. Uma retentativa honesta só parte depois que a resposta anterior chegou; uma duplicata é concorrente com a original, ou posterior a um lance já encerrado. A marca de "em voo" no Redis deixa de ser otimização de corrida e passa a ser a **definição de duplicata** nesta API.

---

### 32. A chave é um UUID, validada no middleware como `X-User-Id` é

`X-Idempotency-Key` mal formado devolve `400 invalid_idempotency_key`. Ausente é legítimo e vira passagem.

**Por quê:** sem impressão digital do corpo (decisão 31), a única defesa contra dois clientes honestos escolherem a mesma chave é o espaço de chaves. UUID resolve isso, e de quebra dá um teto de tamanho de graça — sem ele, um cliente poderia gravar megabytes por requisição no Redis. É a mesma validação que `RequireUserID` já faz, com o mesmo custo e o mesmo formato de erro.

**Sem escopo por usuário:** a alternativa era guardar sob `idem:<user>:<chave>`. Ela foi descartada porque `X-User-Id` não é autenticado — escopar por uma identidade que o cliente escolhe não isola nada contra quem seja hostil, e contra quem seja honesto o UUID já isolou. Escopar também obrigaria a coluna `bids.idempotency_key` a guardar a chave composta, para que o índice único e o Redis concordassem sobre o que é a mesma chave.

---

### 33. Duplicata em voo recebe `425`, imediatamente — nunca `409`, e nunca esperando

Emenda [provas.md §3](../projeto/provas.md), que registrava *"o segundo request aguarda ou recebe `409 Idempotency-In-Flight`"*. As duas metades caem.

**Por que não `409`:** esse código já significa `version_conflict` neste contrato. O apostador do k6 conta todo `409` em `bids_conflict` e absorve `currentVersion` e `minNextBid` do corpo — coisas que uma resposta de idempotência não tem, porque nenhuma engine foi consultada. Seria a decisão 2 quebrada e, pior, a série mais central da tese contaminada com eventos que não são disputa: um contador que mistura *"alguém passou na sua frente"* com *"você mandou a mesma coisa duas vezes"* não mede amplificação de retentativa, mede o gerador.

**Por que não esperar:** esperar significa segurar a segunda requisição até a primeira terminar, e a primeira demora o que a **engine** demora. Sob a pessimista com o lock disputado, a duplicata herdaria os mesmos 300ms de espera. O custo da idempotência deixaria de ser constante entre as três estratégias e passaria a escalar com a latência de cada uma — o middleware entrando no eixo de comparação pela porta dos fundos, que é exatamente o que a decisão 23 existe para impedir. O curto-circuito imediato custa os mesmos dois round-trips de Redis em qualquer engine.

`425 Too Early` com `error: "idempotency_in_flight"`, `retryable: true`, no envelope sem estado. É um código que o k6 ainda não espera, e não precisa esperar nesta spec: quem manda duplicata é a spec 03, e é ela que ensina o `expectedStatuses`.

---

### 34. Só o `201` é guardado, e o replay é byte a byte, declarado num header

O campo `done` da entrada só nasce no aceite. Rejeição libera a marca em voo e não guarda resposta.

**Por quê:** rejeição é idempotente por natureza. Um `422` reenviado continua `422`, porque `minNextBid` só cresce; um `409` reenviado continua `409`, com a versão ainda mais velha; `410` e `404` são propriedades do leilão, não da requisição. Guardá-las custaria memória para devolver o que a engine devolveria de qualquer forma — e o que a idempotência existe para impedir, a **segunda linha em `bids`**, só pode nascer de um aceite.

**O replay é verbatim.** Um replay que recalculasse o corpo a partir do estado de agora devolveria o `minNextBid` de agora dentro da resposta de um lance de antes, e o cliente não teria como saber qual dos dois números leu. Reconstruir o envelope pedindo à engine, ou lendo `bids` pela chave, foi descartado por isso e por custar round-trip ao Postgres no caminho que existe para não ter custo.

**A marca vai num header, `X-Idempotency-Replayed: true`,** e não num campo do corpo: o corpo tem que permanecer idêntico, e um campo a mais nele seria a diferença que esta decisão acabou de proibir.

---

### 35. O claim é um script Lua, e a entrada tem dois TTLs

Um `EVALSHA` na entrada, um pipeline na saída. Dois round-trips de Redis por lance com chave, zero sem chave.

**Por quê:** o claim precisa fazer quatro coisas indivisíveis — ver se há resposta guardada, ver se há requisição em voo, incrementar a contagem de tentativas e tomar a marca. A alternativa era compor comandos avulsos (`SET NX`, `GET` quando o `NX` falha, `INCR`), o que custa dois a três round-trips e coloca o `INCR` do lado errado: ele contaria também as duplicatas barradas, que nunca chegaram à engine e portanto não são tentativas de nada. Contar tentativa no mesmo passo atômico em que se concede a passagem é o que faz `bid_attempts_per_accept` significar *"requisições que chegaram à engine sob esta chave até uma ser aceita"*.

**Os dois TTLs respondem a perguntas diferentes.** Em voo, 30s — o número que provas.md já tinha fixado — é o teto do estrago de um processo que morra entre o claim e o finish: a chave fica presa por 30s em vez de para sempre, e como `BID_DEADLINE` é 2s nenhuma requisição viva é atingida. Terminal, 5min, é maior que o cenário mais longo do projeto (`ramp`, 2min), então dentro de uma célula nenhuma entrada terminal expira e nenhuma duplicata escapa por vencimento.

O finish vai num `defer`: se o handler entrar em pânico, o `gin.Recovery()` que está por fora só recupera depois, e a chave não fica ocupada por 30s por causa de um bug.

---

### 36. Redis fora do ar falha fechado, e `/readyz` ganha a terceira condição

Erro de Redis no claim vira `503 unavailable`, `retryable: true`, com uma linha de log. `/readyz` passa a checar `database`, `schema` e `redis`; `/healthz` não muda.

**Por quê:** falhar aberto — deixar o lance passar sem idempotência — seria o sistema fazendo uma **promessa mais fraca em silêncio**, que é a coisa que a decisão 8 existe para proibir. E nem sequer evitaria o erro: sem a marca em voo, as duplicatas concorrentes chegariam às engines, ambas escreveriam, e o índice único parcial recusaria a segunda com violação de unicidade — `503` de qualquer forma, só que depois do trabalho gasto e com a transação abortada.

`/healthz` continuar verde é a regra da etapa 1: infraestrutura divergente não é processo morto, e reiniciar não conserta. Também é o que o cenário de caos da etapa 4 vai cobrar, quando o Redis for pausado por 5 segundos e o compromisso registrado for *"requisições falham de forma limpa"*.

**O que conta como hit:** só duplicata efetivamente barrada. Redis caído produz zero hits, e essa é a verdade — nada foi barrado. É o mesmo critério que a spec 01 aplicou a `lock_wait_duration_seconds`: falha de infraestrutura não vira amostra.

---

### 37. O middleware é a segunda fronteira única, e por isso pode carregar o label `strategy`

`idempotency_hits_total{strategy,kind}` e `bid_attempts_per_accept{strategy}` são observadas no middleware. Emenda [observabilidade.md](../projeto/observabilidade.md), que declarava a primeira sem label.

**Por quê:** a decisão 23 proíbe que cada engine se meça, porque nada no compilador impediria uma delas de começar o cronômetro num ponto mais generoso que as outras. O middleware não é esse caso: ele está **acima** do switch de estratégia, é um componente só, idêntico para as três, e não sabe nem pode saber qual engine está atrás dele. Não é cada uma se medindo — é uma segunda fronteira única, e a regra continua de pé.

O label existe pela mesma razão mecânica de `bid_outcomes_total{strategy}`: sem ele, o dashboard da etapa 5 não consegue sobrepor três curvas vindas de 36 execuções separadas. É o oposto do caso de `lock_wait_duration_seconds` (decisão 28), que não leva label porque só uma engine a produz.

**`kind` em vez de duas séries:** `replayed` e `in_flight` são duplicatas barradas por caminhos diferentes, e quem quisesse o total teria que somar duas séries e esqueceria uma. É a mesma manobra pela qual `bid_conflicts_total` não existe. Os dois filhos são ligados no boot, como o `confirm` da etapa 1, para que `/metrics` nunca pareça ter esquecido a série quando o que houve foi ausência de duplicata.

**Buckets de 1 a 10** em `bid_attempts_per_accept`, porque `MAX_RETRIES` é 10 (decisão 18) e nada pode cair acima do último. O `+Inf` fica sendo, de graça, o detector de um cliente que violou o próprio limite.

---

### 38. A série do k6 não some: ela é renomeada, e as duas medições convivem

`bid_attempts_per_accept` do k6 vira `client_attempts_per_accept`, e sai em `client.json` como `clientAttemptsPerAccept`. O nome publicado fica com a série do servidor.

**Por quê:** observabilidade.md prometeu migração, e migração literal jogaria fora um dado. O k6 mede *quantas tentativas o cliente fez*; o servidor mede *quantas chegaram*. Sob 1000 VUs contra a pessimista, um VU desiste em `BID_DEADLINE` enquanto o servidor ainda segura a requisição: a tentativa existiu para um e não para o outro, e a diferença entre as duas curvas é essa perda.

Renomear é o que impede o acidente óbvio: duas medições diferentes com o mesmo nome em lugares diferentes é a forma mais garantida de alguém plotar a errada.

**Consequência verificável:** `cmd/checker` não lê nenhum dos dois campos — `clientReport` nunca teve `attemptsPerAccept` —, então a renomeação não toca o checker, e isso sai num `git diff --name-only`.

---

### 39. As engines gravam `bids.idempotency_key`, e a conformidade passa a exigir isso

`INSERT` com `NULLIF($n, '')` nas duas engines. A suíte ganha um caso: aceite com chave grava aquela chave, aceite sem chave grava `NULL`.

**Por quê:** a decisão 15 chamou o índice único parcial de *"a rede embaixo, para quando o Redis reiniciar ou uma chave expirar cedo"*. Ele existe desde a migration `001` e nunca recebeu um valor. Uma rede que ninguém tece não é rede, e o invariante de chave repetida que a spec 03 vai acrescentar ao `cmd/checker` seria vácuo puro numa coluna toda `NULL`.

O `NULLIF` é o que mantém o desenho da decisão 15 intacto: lance sem chave continua fora do índice parcial.

**Por que na suíte e não só nas engines:** o alvo é o shard da etapa 3, que decide em memória e não tem `WHERE` nenhum para lembrá-lo da coluna. Com o caso na suíte, a obrigação é executável — a engine nova passa ou está errada. Isso não asserta SQL contra uma engine específica: é a mesma leitura da história de volta que a decisão 24 já faz, aplicada a mais uma coluna. A assinatura de `RunConformance` não muda.

---

### 40. O aperto do I5 é da spec 03, e a spec 02 entrega a condição

`cmd/checker` continua tratando `dbCount > client.Accepted` como aviso durante toda a spec 02. O comentário em `cmd/checker/client.go` que promete *"etapa 2 drops that tolerance"* se cumpre na spec 03, não aqui.

**Por quê:** o I5 só pode apertar quando o cliente conseguir contabilizar **todo** `201` durável. Com idempotência, isso exige duas coisas que não existem nesta spec:

- o k6 precisa retentar sobre **erro de transporte** com a mesma chave, para que um `201` cuja resposta se perdeu volte como replay em vez de sumir. Hoje o apostador conta `bids_error` e desiste
- o k6 precisa contar o replay **separado** do aceite, ou cada duplicata reenviada viraria um `bids_accepted` a mais e o I5 falharia pelo lado oposto, acusando lance sumido onde houve lance contado duas vezes

As duas são mudanças no `bench/bid-storm.js`, e o k6 é o assunto da spec 03 — a mesma spec que injeta as duplicatas, ou seja, a mesma que torna a tolerância errada. Apertar o I5 aqui seria apertar um invariante contra um cliente que ainda não consegue nem violá-lo nem satisfazê-lo, e a primeira célula da spec 03 falharia por um motivo que pertence a ela.

**O que esta spec entrega em vez do aperto:** o header `X-Idempotency-Replayed`, que é a única forma de o cliente distinguir um replay de um aceite fresco. Sem ele, o aperto é impossível em qualquer spec.

---

### 41. O custo do Redis é novo e é constante — e "sem idempotência" é escolha do cliente, não variável de ambiente

Dois round-trips de Redis entram no caminho quente das três engines. Não entra nenhuma variável para desligá-los.

**Por quê:** o custo é idêntico entre as estratégias, então não move o cruzamento das curvas. Mas move o **nível** das três, e isso tem uma consequência que precisa estar escrita: nenhum número medido antes desta spec pode aparecer no mesmo gráfico que um medido depois. As células `e2c5-pess` e `e2c5-otim` da spec 01 continuam valendo pelo que eram — a prova de que a engine funciona ponta a ponta — e não como linha de base de latência. A matriz da etapa 5 roda inteira depois desta spec, de uma vez, como a decisão 13 já exigia por outro motivo.

**Sem `IDEMPOTENCY=on|off`:** ela já é desligável do lado certo, que é **não mandar o header**. Um lance sem chave não toca o Redis, então a célula de controle "sem idempotência" custa zero em configuração e vive no gerador de carga, onde as outras escolhas de cliente já vivem. Uma variável de ambiente seria um quinto eixo na matriz respondendo a uma pergunta que este projeto não fez.

**Consequência de método:** o `docker compose` passa a limitar CPU e memória do Redis, e `bench/env.sh` passa a gravar esses limites junto dos outros três. O terceiro invariante de método pede recursos fixos e iguais, e o Redis acabou de entrar no caminho medido — um serviço sem limite num host carregado é uma variável escondida dentro de um número publicado.

---

## Decisões da spec 03

Tomadas ao desenhar a [spec 03 da etapa 2](../specs/etapa-2/03-spec-duplicatas-e-invariantes.md), que demonstra a idempotência sob carga e fecha os dois invariantes adiados pela decisão 40.

### 42. A coorte de duplicação é 10% dos lances lógicos, não 10% das requisições brutas

Uma função determinística de `(__VU, __ITER)` seleciona uma em cada dez iterações antes de qualquer tentativa ou resposta. Metade da coorte recebe duplicata concorrente e metade recebe replay posterior.

**Por quê:** o otimista produz mais requisições por lance por causa dos retries. Selecionar 10% das requisições já amplificadas faria a quantidade de duplicatas depender da engine, transformando o mecanismo medido em parâmetro do gerador. O lance lógico é a unidade estável e já é a unidade nomeada pela chave.

**Emenda:** [provas.md](../projeto/provas.md) diz “10% das requisições”. Leia-se 10% dos lances lógicos, com `duplicatesSelected` e `duplicatesInjected` publicados para que a taxa efetiva nunca seja presumida.

### 43. A duplicata é uma requisição extra, não uma tentativa do apostador

`bids_attempts` e `client_attempts_per_accept` não contam a cópia injetada. Ela tem o contador próprio `duplicates_injected`. A duplicata concorrente usa `http.batch`, e o par de respostas é reduzido por semântica: `201` resolve, `425` não carrega estado e, entre rejeições, vence a maior `currentVersion`.

**Por quê:** tentativa mede quantas vezes o apostador precisou re-mirar. Duplicata mede a perturbação controlada que exercita o middleware. Misturar as duas destruiria tanto a amplificação quanto a taxa de injeção. Se uma cópia escapar da janela de `busy` depois de uma rejeição e alcançar a engine, o histograma do servidor a conta — porque para o servidor ela foi, de fato, uma tentativa.

### 44. `accepted` conta um lance lógico, e replay tem contador próprio

O primeiro `201` observado sob uma chave incrementa `bids_accepted` uma vez, tenha ou não `X-Idempotency-Replayed: true`. Todo `201` com esse header também incrementa `bids_replayed`; respostas posteriores sob a mesma chave nunca incrementam `accepted` de novo.

**Por quê:** a spec 02 passou a permitir dois `201` para uma linha — o fresco e o replay byte a byte. Contar respostas brutas faria o checker acusar write perdido exatamente quando a idempotência funcionou. Contar o fato lógico preserva a igualdade entre cliente, outcome da engine e banco.

### 45. Erro de transporte retenta com a mesma chave; `503` não

`status == 0` incrementa erro e `transport_retries`, aplica a política vigente e continua dentro de `MAX_RETRIES` e `BID_DEADLINE`, sem trocar a chave. Se o commit aconteceu, a tentativa seguinte recebe replay; se a requisição nunca chegou, recebe passagem.

**Por quê:** o cliente não tem informação para distinguir as duas situações, e a chave existe justamente para resolver a ambiguidade sem adivinhação. `503` continua terminal: retentá-lo mudaria a carga durante uma falha de infraestrutura e pertence aos cenários de caos da etapa 4.

### 46. I5 passa a exigir igualdade exata

`count(bids) == client.accepted` e `max(seq) == client.maxSeqSeen`. Desigualdade em qualquer direção é `FAIL`; o aviso para banco à frente deixa de existir.

**Por quê:** a tolerância da etapa 1 existia porque uma resposta durável podia sumir. O replay entregue na spec 02 e consumido nesta remove a justificativa. Banco atrás é write perdido; banco à frente é resposta ambígua não reconciliada ou contagem lógica errada. Nenhuma das duas células pode virar resultado publicado.

### 47. I7 exige uma chave presente e distinta por linha

O checker compara `count(*)`, `count(idempotency_key)` e `count(DISTINCT idempotency_key)`; os três precisam ser iguais.

**Por quê:** o índice parcial já impede duas chaves não nulas iguais, mas não impede uma engine de esquecer a coluna e escrever `NULL`, que escapa do índice por definição. I7 prova a célula real em vez de apenas confiar no DDL. O teste planta uma chave nula e não remove a restrição para fabricar um estado impossível em produção.
