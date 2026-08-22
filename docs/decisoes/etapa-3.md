# Decisões — Etapa 3

[← índice](../README.md)

Registro das decisões tomadas ao desenhar as specs da etapa 3. A numeração continua a da [etapa 1](etapa-1.md) e da [etapa 2](etapa-2.md): as decisões de 1 a 47 continuam valendo, e as que são emendadas aqui dizem em qual ponto.

---

## Decisões da spec 01

Tomadas ao desenhar a [spec 01 da etapa 3](../specs/etapa-3/01-spec-engine-single-writer.md), a engine single-writer. Duas delas **emendam** o que já estava publicado em `projeto/estrategias.md` e `projeto/observabilidade.md`. Onde houver divergência, vale o que está aqui.

---

### 48. O lote só cresce entre leilões distintos, e na célula de 1 leilão ele vale 1

Emenda [estrategias.md](../projeto/estrategias.md#single-writer), que descreve o ganho como *"agrupar centenas de lances em um commit"*.

**Por quê:** a frase é verdadeira entre leilões e falsa dentro de um. Dentro de um leilão, dois lances aceitos em sequência são separados por um round-trip de cliente: o shard aceita o primeiro, e todo comando que já estava no inbox para aquele leilão passa a estar abaixo de `minNextBid` e é **rejeitado em memória**. O próximo aceite daquele leilão só pode chegar depois que alguém leu a resposta e re-mirou — e a resposta do aceite só sai depois do commit. Logo, na célula de 1 leilão, o lote tem tamanho 1 por construção, e não existe `fsync` para amortizar.

O ganho ali é outro, e é grande: **perder é de graça**. Uma rejeição no shard não toca o banco. O otimista paga um `SELECT` de classificação por rejeição (decisão 1) e o pessimista paga `Begin` + `SELECT ... FOR UPDATE` + `Rollback`. Sob 1000 VUs num leilão, as rejeições são a esmagadora maioria das requisições, e é aí que as três engines divergem em custo por requisição — não no aceite.

**Consequência que precisa ser encarada:** a engine tem dois ganhos diferentes em duas células diferentes, e o gráfico da etapa 5 vai mostrar isso separado — com 1000 leilões o lote amortiza o `fsync`; com 1 leilão o lote é 1 e o que amortiza é a rejeição sem banco. Registrar agora importa porque a spec 02 vai publicar `shard_batch_size` e encontrar ~1 na célula de manchete. Número que surpreende o autor depois do fato costuma virar explicação improvisada.

---

### 49. O estado é hidratado sob demanda, na goroutine do shard, e a ausência não é cacheada

Miss no mapa → um `SELECT` síncrono, feito pela própria goroutine dona do shard. Leilão inexistente devolve `NotFound` e **não** vira entrada negativa.

**Por quê:** pré-carregar tudo no boot não funciona neste sistema. `cmd/seed` trunca e re-semeia entre células com o `auctiond` de pé, e `POST /auctions` cria leilão em tempo de execução; qualquer cache montado no boot estaria errado na primeira célula. A hidratação preguiçosa é a única forma de o shard ser dono de um estado que ele não criou.

**Síncrona, e não em paralelo:** buscar o estado numa segunda goroutine devolveria o comando ao inbox depois de outros já terem sido decididos, ou exigiria uma fila de espera por leilão. As duas alternativas criam um segundo caminho de decisão dentro da engine cuja premissa é ter **um** — e a ordem total por leilão, que é o que dá `seq` sem buraco de graça, passaria a depender de código em vez de depender da estrutura. O custo é um round-trip bloqueando aquele shard, uma vez por leilão, e o warmup de `run-cell.sh` já o paga antes da célula medida.

**Sem cache negativo:** guardar "não existe" faria um leilão criado depois pelo `POST /auctions` ficar invisível enquanto a entrada durasse. O preço é um round-trip por lance para leilão inexistente, que na matriz é zero — o gerador só conhece leilões do manifesto.

**Limite registrado:** o mapa não expira nada. Num processo que roda 36 células com 1000 leilões cada, sobram ~36 mil entradas de poucos bytes, e o processo morre no fim. Num sistema de verdade isso é vazamento, e a correção óbvia — despejar por `ends_at` vencido — fica registrada como não feita, porque não muda o gráfico.

---

### 50. O relógio do banco chega ao shard como um offset medido na hidratação

A leitura de hidratação devolve `clock_timestamp()`. O shard guarda `skew = dbNow - localMid`, e toda decisão de fechamento usa `time.Now().Add(skew)`.

**Por quê:** a decisão 22 fixou que `ends_at` é decidido pelo relógio do Postgres, e a 27 corrigiu qual função usar dentro de transação. O shard não pode cumprir nenhuma das duas literalmente: perguntar as horas ao banco no caminho quente seria um round-trip por lance, ou seja, exatamente o que a engine existe para não fazer. Decidir com o relógio do contêiner seria a terceira engine com uma guarda de fechamento diferente das outras duas, e a diferença apareceria na borda do `ends_at` — o cenário inteiro do projeto — parecendo diferença entre estratégias.

O offset resolve isso com o round-trip que **já** acontece. `localMid` é a média entre o instante anterior e o posterior à consulta, então o erro do offset é no máximo meio round-trip da hidratação, tipicamente algumas centenas de microssegundos. É menor que a diferença que ele existe para eliminar.

**Limite registrado:** o offset é medido uma vez por hidratação e não acompanha deriva de relógio dentro da célula. Uma célula dura dois minutos; NTP não move um relógio o suficiente nesse intervalo para mudar um desfecho. Se um dia mover, o sintoma é I4 no `cmd/checker`, que compara o que o banco escreveu com o que o banco tinha marcado.

---

### 51. `created_at` é escrito pelo shard, e é o instante da decisão

O `INSERT` do lote passa `created_at` explicitamente, em vez de deixar o `DEFAULT now()` da migration `001`.

**Por quê:** nas outras duas engines, decidir e comitar acontecem no mesmo statement, então `now()` é o instante da decisão por acidente feliz. No shard não: o lote comita depois, e `DEFAULT now()` carimbaria o instante do commit. Um lance decidido legitimamente 300 microssegundos antes de `ends_at` seria gravado com `created_at` depois de `ends_at`, e **I4 acusaria lance após o fechamento num lance que o servidor aceitou dentro do prazo**. O invariante estaria certo sobre a coluna e errado sobre o mundo.

Escrever o instante da decisão também dá significado à coluna: `created_at` passa a ser *quando o shard decidiu*, e a diferença entre ela e o instante do commit é exatamente o custo da durabilidade, que é o que a spec 02 vai publicar como série. Sem isso, a coluna guardaria a mesma informação duas vezes e a série não teria contra o que ser conferida.

O valor gravado é o instante da decisão **corrigido pelo offset da decisão 50**, ou seja, expresso no relógio que decide `ends_at`. Comparar duas colunas escritas por relógios diferentes seria o mesmo erro em outro lugar.

---

### 52. O lote fecha por lotação, por inbox vazio ou por linger de 1ms — o ticker de 2ms sai

Emenda o `time.NewTicker(2 * time.Millisecond)` de [estrategias.md](../projeto/estrategias.md#single-writer).

**Por quê:** um tick fixo cobra o atraso de todo mundo para amortizar o `fsync` de alguns. Sob carga baixa — a célula de 1000 leilões no início do ramp — o inbox esvazia entre lances, e o ticker faria cada lance esperar até 2ms por um lote que nunca vai crescer. Seriam 2ms somados a `bid_confirm_duration_seconds` por decisão de implementação, não por mecanismo, e apareceriam no gráfico como custo da estratégia.

As três condições cobrem os três regimes sem parâmetro novo:

| Condição | Regime que ela atende |
| --- | --- |
| `len(batch) >= 256` | saturação: o lote fecha por tamanho e o `fsync` é amortizado |
| inbox vazio | carga baixa: comita na hora, latência igual à das outras engines |
| lote aberto há mais de 1ms | contenção num leilão só: o aceite não pode ficar preso atrás de mil rejeições |

A terceira é a que não é óbvia, e é a que a decisão 48 torna necessária: na célula de 1 leilão o inbox **nunca** esvazia e o lote **nunca** chega a 256, porque quase tudo que entra é rejeição. Sem o linger, o único aceite do lote esperaria indefinidamente.

**Por que 1ms:** o linger existe para amortizar um commit, então ele não pode custar mais do que o commit que amortiza. Um round-trip de commit com `synchronous_commit=on` no compose fica na mesma ordem de grandeza, e é essa a regra registrada — não o número. Um linger adaptativo, igual ao custo do último commit, foi considerado e descartado: introduz realimentação entre carga e latência, e um número que se move sozinho é um número que ninguém reproduz.

---

### 53. Um statement por lote, e `UNIQUE (auction_id, seq)` é a asserção que aborta

O commit é um `INSERT ... SELECT unnest(...)` mais um `UPDATE ... FROM unnest(...)` numa CTE só, sem `Begin`/`Commit` explícitos e sem guarda de versão no `UPDATE`.

**Por quê:** transação explícita seriam quatro round-trips por lote. Num lote de 256 isso é irrelevante, mas na célula de 1 leilão o lote é 1 (decisão 48) e a terceira engine passaria a pagar quatro round-trips por lance aceito exatamente na célula de manchete — aleijada por decisão de implementação, que é o mesmo erro que a decisão 19 evitou do outro lado. Um statement é atômico do mesmo jeito e custa um.

**Sem guarda de versão, de propósito.** A tentação é escrever `WHERE version = prev_version` para detectar um segundo escritor. Ela é armadilha: a guarda falha silenciosamente para aquele leilão, o `INSERT` do mesmo lote grava a linha assim mesmo, e o resultado é escrita parcial comitada — pior que o problema. Verificar em Go depois não adianta: o statement já comitou.

O que já existe faz o trabalho melhor. Se a memória do shard divergir do banco, o `seq` que ele calculou já pertence a outra linha, e `UNIQUE (auction_id, seq)` **aborta o statement inteiro** — atomicamente, sem escrita parcial, sem código novo. É a restrição que o [schema.md](../projeto/schema.md) chama de asserção viva, e é a primeira vez que ela tem alguém para pegar: o shard é a única engine que calcula `seq` fora do banco.

**O que acontece depois do erro:** todo comando do lote recebe erro (o handler devolve `503`) e os leilões daquele lote são **despejados do mapa**. Manter em memória um estado que o banco não tem faria o próximo aceite gravar um `seq` com buraco atrás, e I1 reprovaria a célula. Despejar força a re-hidratação a ler a verdade, seja ela qual for.

**Limite registrado, e é feio:** se o commit tiver sucesso e a resposta se perder, as linhas estão no banco e os clientes receberam `503`. Com a chave de idempotência da decisão 31, a retentativa daquele lance re-mira mais alto sob a **mesma chave**, e o índice único parcial recusa o segundo `INSERT` — derrubando o lote inteiro, de todo mundo, não só de quem tinha a chave. A cadeia exige perder a conexão no meio de um commit, e uma célula que perdeu conexão já é uma célula inválida que I5 vai reprovar. A correção conhecida — bissecar o lote e recomitar sem a linha ofensora — fica registrada como não feita, porque complexidade que só se paga num evento que ainda não aconteceu é complexidade que ninguém testa.

---

### 54. Depois de entrar no inbox, o comando é decidido, comitado e respondido

O cancelamento do chamador é respeitado **antes** do enfileiramento e ignorado depois. O commit roda sob um contexto do processo, com timeout próprio, nunca sob o contexto de um chamador.

**Por quê:** antes de entrar no inbox o comando não é nada, e desistir é grátis — `select` entre o envio e `ctx.Done()`, e quem desistiu recebe erro sem custo nenhum. Depois de decidido, o comando **já é história**: consumiu um `seq`, moveu o `highest_bid_cents` em memória e mudou o que os próximos lances daquele leilão vão precisar superar. Retirá-lo do lote deixaria um buraco na sequência que I1 reprova; deixá-lo no lote e abandonar a resposta é o mesmo trabalho com uma resposta a menos.

**O commit não pode ser de ninguém:** um lote é de muitos chamadores. Se ele rodasse sob o contexto de um deles, um VU que desistiu por `BID_DEADLINE` cancelaria a durabilidade dos outros 255 — cada um deles com o próprio `201` prometido. O contexto do commit é do processo, com timeout de 5 segundos, que é maior que qualquer commit numa célula viva e menor que a paciência de qualquer cliente.

**Efeito colateral bom:** como a espera pela resposta não tem saída antecipada, `bid_confirm_duration_seconds` do shard mede sempre a decisão completa, e nunca o instante em que um cliente desistiu. As três engines continuam medindo a mesma coisa na mesma fronteira.

---

### 55. O shard decide ou comita, nunca os dois ao mesmo tempo

O commit acontece dentro do laço da goroutine, bloqueando-a. Nada é decidido enquanto o lote está no banco.

**Por quê:** pipelinar — decidir o lote seguinte enquanto o anterior comita — é a otimização óbvia e é onde está o próximo ganho desta engine. Ela também dobra o número de estados que o shard precisa distinguir: passa a existir "decidido, no lote em voo" além de "decidido, no lote aberto" e "durável", e o despejo da decisão 53 precisa saber a qual dos dois a falha pertence. Com o commit síncrono, a janela entre decidido e durável é **exatamente um lote**, e essa frase é verificável em vez de ser aproximada.

O custo é o shard ocioso durante o round-trip do commit. Ele é limitado porque o lote já amortiza — 256 lances por round-trip — e porque são oito shards independentes: enquanto um comita, os outros sete decidem.

Fica registrado como não medido: este projeto não afirma nada sobre o ganho do pipeline. Afirmar sem medir é o que os quatro invariantes de método existem para impedir.

---

### 56. Oito shards, inbox de 1024, lote de 256: constantes, não variáveis de ambiente

Nenhum dos três é lido do ambiente, e nenhum entra em `env.json` além do commit que os fixa.

**Por quê:** toda variável de ambiente nova é um eixo experimental que alguém vai querer varrer, e a regra de escopo do projeto é uma pergunta, três implementações, um benchmark. O único sweep planejado é o de `DB_POOL_SIZE` na etapa 5, e ele existe porque o pool é a história do pessimista (decisão 7). Sweep de shard seria a etapa 5 medindo o ajuste da terceira engine em vez das três estratégias.

**Por que oito:** cada shard segura no máximo uma conexão por vez — ou hidratando, ou comitando —, então oito shards contra `DB_POOL_SIZE=25` deixam folga confortável para as rotas de leitura e para `/readyz`, e nunca transformam o pool em fila do shard. Acima disso, a disputa migraria para o `pgxpool` e o custo do mecanismo ficaria indistinguível de pool subdimensionado, que é precisamente o erro que a decisão 7 existe para impedir. Abaixo, os dois núcleos do `auctiond` ficariam ociosos na célula de 1000 leilões.

**Por que o inbox é limitado:** um canal sem teto é uma fila de memória escondida dentro do processo, e o sistema pareceria absorver carga que na verdade acumulou. Com 1024 por shard, os 1000 VUs do cenário mais pesado cabem sem bloquear ninguém — o teto existe para o dia em que não couberem, e nesse dia ele aparece como espera no enfileiramento, medida pela profundidade do inbox que a spec 02 publica.

---

### 57. A rejeição responde do estado decidido; só o `201` espera a durabilidade

O envelope de `409`, `422` e `410` sai do estado em memória, que pode estar um lote à frente do banco. Nenhum `201` sai antes do commit.

**Por quê:** segurar rejeição até o lote comitar acrescentaria latência de durabilidade à resposta que **não tem nada para tornar durável** — e na célula de 1 leilão isso é a quase totalidade das respostas. A engine ficaria mais lenta para preservar uma promessa que ninguém fez: o invariante de método é *`201` significa durável*, e ele continua intacto.

**A assimetria que isso cria, dita em voz alta:** a rejeição do shard carrega o estado mais fresco que existe, enquanto a do otimista carrega o último estado **comitado** lido pelo `classify`. O apostador re-mira melhor contra o shard e desperdiça menos tentativas. Isso é vantagem real do mecanismo — quem tem um escritor único sabe a verdade antes de ela ser durável — e não uma promessa mais fraca. Fica registrado porque é exatamente o tipo de coisa que um revisor hostil encontraria sozinho, e é melhor que ele a encontre escrita.

`GET /auctions/:id` continua lendo o banco e publicando o estado durável, ou seja, pode ficar até um lote atrás do que o shard já decidiu. É a mesma regra vista do outro lado: a rota de leitura publica fato durável, e o único lugar onde o estado decidido aparece é a resposta de quem provocou a decisão.

---

### 58. Esta spec não publica série nova, e isso emenda a decisão 16 por uma spec

`bid_accept_duration_seconds`, `shard_inbox_depth`, `shard_batch_size` e `journal_lag_seconds` ficam para a spec 02 da etapa 3. A engine entra medida pelo decorator, como as outras duas: `bid_confirm_duration_seconds{strategy="shard"}` e `bid_outcomes_total{strategy="shard"}`.

**Por quê:** a decisão 16 diz que métrica entra junto da engine que a alimenta, e a razão dela é não haver painel vazio sem se saber se é bug ou desenho. Aqui a razão não se aplica: a engine nasce no mesmo eixo de comparação das outras duas, e o que falta é a instrumentação **do mecanismo**, que não tem par nas outras.

E há um motivo técnico para não empurrar `bid_accept_duration_seconds` para dentro desta spec. O gap entre aceite e confirmação só é legível se as duas séries observarem a mesma população, e não observam: `confirm` agrega todos os desfechos, e o custo de durabilidade só existe nos aceites. Sob contenção alta as rejeições dominam, os dois p95 caem na mesma população de rejeições e o gap lido no gráfico seria perto de zero — **subestimando o custo da durabilidade, a favor da tese do projeto**. O instrumento honesto é outro: um histograma por lance aceito, medido dentro do shard, da decisão até o commit. Ele resolve a mesma pergunta sem subtrair séries, e é ele que a spec 02 publica, junto de profundidade e tamanho de lote, onde a questão da população pode ser resolvida uma vez para as quatro.

**Limite desta spec, então:** o argumento central do desenho — durabilidade custa isto aqui, e está exposto em vez de escondido no contrato — fica **afirmado e não medido** até a spec 02. O que esta spec prova sem série nova é o outro lado: que o lote existe e amortiza, contando transações em `pg_stat_database` antes e depois da célula.
