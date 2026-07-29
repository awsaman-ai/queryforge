## Project Goals
- The goal of this project queryForge is to replace all traditional search to a modern AI based search.
- almost 50% websites in the internet have a page where they can see any kind of details weather in json/table/word/excel and main important thing is they have so called filters in it, where they can search/apply filters/sorting and more. The limitation here is there is no NLP to query, I only search few limited things. I cant make my own query out of this. 
- This project will overcome this issue in much better way by introducing NLP to query with your own type of configuration. 

## Techincal Project Goals: (Phase 1)
- Write this project in go. I can then import this lib and can use in any golang project.
- Have a html document with all the details of configuration, So users can refer that documentation and can create their own configuration file.
- Make architecture simple not too complex. Add proper commenting on each line with purpose.

## Configuration options:
- You have to give configuration options in very simple understandable way. Make sure you only accept json as of now. 
- This is totally related to database. Either there is not connection to database we are only giving the query but in configuration include things like weather this field is indexed or not, which filed to take on priority, inculde/exclude fields, have field level configuration like what type of operation this field can perform sorting/searching. 
- Make sure this is only GET operations.
- Based on SQL and NoSQL study the search pattern of this DB's and create best configuration files. Later you also have to add this on html doc so users can study and can apply this things.

## Things to make sure while creating this project: (Phase 1)
- After every component build dont go ahead to blindly build another stuff. Stop there you have to do QA on all happy and worst case scenarios. 
- Do a bad testing, try breaking the system. 
- Make sure to report bugs and solve that. 
- while doing all this things you have to maintain an excel file with details and status of all the bugs/fixes you have done or doing.
- If doubt on any kind of feature or component dont assume, you have to ask to me. 

