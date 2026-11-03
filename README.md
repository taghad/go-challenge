> **Estimation Service.** A Go and Gin service that estimates the users in a segment over the
> last 14 days, with Redis HyperLogLog. Only Docker is needed.
>
> ```sh
> docker compose up --build                             # then open http://127.0.0.1:8080
> docker compose run --rm test                          # run the tests
> docker compose run --rm uss -rate 1000 -duration 30s  # send simulated USS traffic
> ```
>
> [Test report](docs/TEST_REPORT.md) (also at http://127.0.0.1:8080/report) ·
> [Design](docs/DESIGN.md) · [Measured results and limits](docs/DESIGN.md#measured-results) ·
> [Decisions](docs/DECISIONS.md)

# How many users? (Go Challenge)

Suppose there is a "User Segmentation Service" (USS) that segments users based on their activities.
For example, if a user visits sports news, USS classifies and tags "sport" to him.
So we have a pair: (the user_id, and the segment) for example, (u104010, "sports").

We want to develop an Estimation Service (ES) that interacts with USS directly.
ES receives the pair from USS as input and stores it.
The responsibility of ES is to answer a simple query: "How many users exist on a specific segment?".
For example, "how many users are in the sports segment?".

![](https://raw.githubusercontent.com/ArmanCreativeSolutions/go-challenge/main/Untitled%20Diagram.drawio.png?raw=true)

The query is simple, but two assumptions may make it a little challenging:
- A specific user remains just two weeks on a segment. After that,
we should not count "u104010" on the sports segment.
- There are millions of users and hundreds of segments. So your solution(s) must be scalable


## Requirements

- Implement a (REST API, RESTful API, soap, Graphql, RPC, gRPC, or whatever protocol you prefer)
interface to receive data (user_id, segment pair) from USS. 
- Implement a method to estimate the number of users in a specific segment. ( `func estimate(segment) -> number of users`)

## Implementation details

Try to write your code as reusable and readable as possible.
Also, don't forget to document your code and clear the reasons for all your decisions in the code.

If your solution is not simple enough for implementing fast, you can just describe it in your documents.

Use any tools that you prefer just explain the reason of choices in your documents.
For example explain why you choose REST API for receiving data.

It is more valuable to us that the project comes with unit tests.

Please fork this repository and add your code to that.
Don't forget that your commits are so important.
So be sure that you're committing your code often with a proper commit message.


## We all use AI. Don't be ashamed

Everyone uses AI for everything. And we expect you to do the same. AI is a big part of our development process, and we care a lot about how you use AI tools during this task.

To earn our immunity after the AI takeover of Earth, we want to make sure you're using it properly.

Add a file named "ai.md" to your project. This file should contain the following information (the more explicit, the better):

1. What AI tools and models did you use?
2. Explain the different stages of software development where you used AI help. Describe how you used your tools at each stage.
3. We want to know how you prompt. So add your prompts to this file too. For each prompt, explain which stage you used it in, how you evaluated the result, and what you did to fix any misbehavior.
4 Explain how you monitor your token usage and what you do to manage it. Link your tools or add your helper prompts.

*There's a sample.ai.md file in the project representing the expected template. Don't forget: "ai.md" is the only file we expect you to write entirely by yourself.*
