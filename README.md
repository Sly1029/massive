This project is meant to provide a modern architecture for developing workflows across providers. 

Each workflow is defined in code, currently done with typescript with its composite nodes and edges. This is then compiled to a proto schema and given to the universal go backend which compiles the target to a datastore (local, S3, R2) and produces a bundle spec, currently just argo although Cloudflare Workers and Temporal planned 

Code is created using LLMs with every line is reviewed. 

Development is done with deno and tsgo
